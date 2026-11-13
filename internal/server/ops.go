package server

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/muthuishere/tmvp/internal/batch"
	"github.com/muthuishere/tmvp/internal/config"
	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
	"github.com/muthuishere/tmvp/internal/protocol"
)

// ValidateOptions wires the connector registry into config validation.
func ValidateOptions() config.ValidateOptions {
	return config.ValidateOptions{
		KnownType: func(t string) bool { _, ok := connector.Lookup(t); return ok },
		Verbs: func(t string) []string {
			var out []string
			for _, v := range connector.VerbsOf(t) {
				out = append(out, v.Name)
			}
			return out
		},
	}
}

// check: validation table + per-target login/secrets/connectivity.
func (e *Engine) check(ctx context.Context, p *product, req protocol.Request, sink protocol.Sink) int {
	cfg := p.config()
	exit := 0
	for _, is := range cfg.Validate(ValidateOptions()) {
		sink(protocol.Frame{Type: protocol.FrameRow, Row: map[string]any{"check": "config", "level": is.Level, "path": is.Path, "message": is.Message}})
		if is.Level == "error" {
			exit = errs.ExitCode(errs.Config)
		}
	}
	ls, err := config.ParseLabels(req.Labels)
	if err != nil {
		sink(protocol.Frame{Type: protocol.FrameError, Error: errs.As(err)})
		return errs.ExitCode(errs.Usage)
	}
	targets, err := cfg.Select(config.Selector{Env: req.Env, System: req.System, Labels: ls})
	if err != nil {
		sink(protocol.Frame{Type: protocol.FrameError, Error: errs.As(err)})
		return errs.ExitCode(errs.CodeOf(err))
	}
	var units []batch.Unit
	for _, t := range targets {
		u := batch.Unit{Env: t.Env, System: t.Name}
		if t.System == nil {
			u.Skip = "not defined in this env"
		}
		units = append(units, u)
	}
	res := batch.Run(ctx, units, func(ctx context.Context, u batch.Unit, em connector.Emit) error {
		s := cfg.System(u.Env, u.System)
		row := map[string]any{"check": "target", "type": s.Type}
		var secs []string
		for _, st := range e.secretStates(ctx, p, cfg, u.Env, s, 0) {
			switch st.State {
			case "unknown", "ok":
				secs = append(secs, st.Name+"="+st.State)
			default:
				secs = append(secs, st.Name+"=failed: "+st.Reason.Message)
			}
		}
		row["secrets"] = secs
		err := e.probe(ctx, p, cfg, u.Env, u.System)
		if err != nil {
			row["connect"] = "failed"
			_ = em.Row(row)
			return err
		}
		row["connect"] = "ok"
		return em.Row(row)
	}, batch.Options{}, sink)
	if res.Exit > exit {
		exit = res.Exit
	}
	return exit
}

func (e *Engine) login(ctx context.Context, p *product, req protocol.Request, sink protocol.Sink) int {
	cfg := p.config()
	s := cfg.System(req.Env, req.System)
	if s == nil {
		sink(protocol.Frame{Type: protocol.FrameError, Error: errs.New(errs.NotFound, "no system %q in env %q", req.System, req.Env)})
		return 2
	}
	key := p.name + "/" + req.Env + "/" + req.System
	if f, ok := connector.Lookup(s.Type); ok {
		if _, isLH := f().(connector.LoginHandler); isLH {
			res := batch.Run(ctx, []batch.Unit{{Env: req.Env, System: req.System}}, func(ctx context.Context, u batch.Unit, em connector.Emit) error {
				cn, err := e.conn(ctx, p, cfg, u.Env, u.System, connector.ModeRead)
				defer p.release(cn)
				if err != nil {
					return err
				}
				return cn.c.(connector.LoginHandler).Login(ctx, connector.Call{Verb: "login", Args: req.Args, Flags: req.Flags}, em)
			}, batch.Options{Timeout: 10 * time.Minute}, sink)
			return res.Exit
		}
	}
	if s.Login == nil || s.Login.Check == "" {
		sink(protocol.Frame{Type: protocol.FrameText, Env: req.Env, System: req.System, Text: "no login declared for this system"})
		return 0
	}
	e.Login.Forget(key)
	if err := e.Login.Ensure(ctx, key, loginSpec(s), cfg.Dir, req.Env, req.System); err != nil {
		sink(protocol.Frame{Type: protocol.FrameError, Env: req.Env, System: req.System, Error: errs.As(err)})
		return errs.ExitCode(errs.CodeOf(err))
	}
	sink(protocol.Frame{Type: protocol.FrameStatus, Env: req.Env, System: req.System, Status: "ok", Text: "logged in"})
	return 0
}

func (e *Engine) secrets(ctx context.Context, p *product, req protocol.Request, sink protocol.Sink) int {
	cfg := p.config()
	p.mu.Lock()
	for k, c := range p.conns {
		p.retire(k, c)
	}
	p.store.Clear()
	p.mu.Unlock()
	if req.Op == protocol.OpSecretsClear {
		sink(protocol.Frame{Type: protocol.FrameText, Text: fmt.Sprintf("secrets cleared for %s", p.name)})
		return 0
	}
	defs := secretDefs(cfg)
	res := p.store.Prefetch(ctx, defs)
	keys := make([]string, 0, len(res))
	for k := range res {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	exit := 0
	for _, k := range keys {
		row := map[string]any{"secret": k, "state": "ok"}
		if err := res[k]; err != nil {
			row["state"], row["reason"] = "failed", errs.As(err).Message
			exit = errs.ExitCode(errs.SecretMissing)
		}
		sink(protocol.Frame{Type: protocol.FrameRow, Row: row})
	}
	return exit
}
