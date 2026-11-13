package server

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/muthuishere/tmvp/internal/config"
	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

// Shared per-target probes used by `tmvp check` (server op) and
// `tmvp doctor` (in-process Diagnose). Secret VALUES never leave the store:
// only names and states are reported.

// SecretStatus is one secret slot a system references.
type SecretStatus struct {
	Name   string      `json:"name"`
	State  string      `json:"state"` // ok | missing | invalid | unknown | skipped
	Reason *errs.Error `json:"reason,omitempty"`
}

// secretStates resolves every secret s references, in parallel, each bounded
// by timeout (0 = only ctx). Order follows config.ReferencedSecrets.
func (e *Engine) secretStates(ctx context.Context, p *product, cfg *config.Config, env string, s *config.System, timeout time.Duration) []SecretStatus {
	names := config.ReferencedSecrets(s)
	out := make([]SecretStatus, len(names))
	var wg sync.WaitGroup
	for i, n := range names {
		out[i].Name = n
		d := cfg.LookupSecret(env, n)
		if d == nil {
			out[i].State = "unknown"
			continue
		}
		wg.Add(1)
		go func(i int, d *config.SecretDef) {
			defer wg.Done()
			sctx := ctx
			if timeout > 0 {
				var cancel context.CancelFunc
				sctx, cancel = context.WithTimeout(ctx, timeout)
				defer cancel()
			}
			if _, err := p.store.Get(sctx, toSecretDef(cfg, d)); err != nil {
				ee := errs.As(err)
				ee.Message = e.Red.Redact(ee.Message)
				out[i].Reason = ee
				out[i].State = "missing"
				if ee.Code == errs.SecretInvalid {
					out[i].State = "invalid"
				}
				return
			}
			out[i].State = "ok"
		}(i, d)
	}
	wg.Wait()
	return out
}

// probe opens env/sys in read mode (login check, via chain, Open) and runs
// the connector's Check when it has one.
func (e *Engine) probe(ctx context.Context, p *product, cfg *config.Config, env, sys string) error {
	cn, err := e.conn(ctx, p, cfg, env, sys, connector.ModeRead)
	if err == nil {
		if ck, ok := cn.c.(connector.Checker); ok {
			err = ck.Check(ctx)
		}
	}
	p.release(cn)
	return err
}

// DiagnoseOptions selects targets and bounds `tmvp doctor`.
type DiagnoseOptions struct {
	Env, System string
	Offline     bool          // no secret commands, no login checks, no connections
	Timeout     time.Duration // per target (default 10s)
}

// TargetDiag is one env × system row of `tmvp doctor`.
type TargetDiag struct {
	Env       string         `json:"env"`
	System    string         `json:"system"`
	Type      string         `json:"type"`
	TypeKnown bool           `json:"type_known"`
	Secrets   []SecretStatus `json:"secrets"`
	Login     string         `json:"login"` // none | ok | required | not checked
	LoginCmd  string         `json:"login_cmd,omitempty"`
	Via       string         `json:"via,omitempty"` // "a>b: ok" | "a>b: failed at a" | ...
	Connect   string         `json:"connect"`       // ok | failed | skipped
	Error     *errs.Error    `json:"error,omitempty"`
}

// Failing reports whether this target has a problem worth exit 1.
func (t TargetDiag) Failing() bool {
	if !t.TypeKnown || t.Login == "required" || t.Connect == "failed" {
		return true
	}
	for _, s := range t.Secrets {
		if s.State != "ok" && s.State != "skipped" {
			return true
		}
	}
	return false
}

// Diagnose runs the doctor probes for every selected target, in parallel.
func (e *Engine) Diagnose(ctx context.Context, configPath string, o DiagnoseOptions) ([]TargetDiag, error) {
	p, err := e.product(configPath)
	if err != nil {
		return nil, err
	}
	cfg := p.config()
	targets, err := cfg.Select(config.Selector{Env: o.Env, System: o.System})
	if err != nil {
		return nil, err
	}
	if o.Timeout <= 0 {
		o.Timeout = 10 * time.Second
	}
	var out []TargetDiag
	for _, t := range targets {
		if t.System != nil {
			out = append(out, TargetDiag{Env: t.Env, System: t.Name, Type: t.System.Type})
		}
	}
	var wg sync.WaitGroup
	for i := range out {
		wg.Add(1)
		go func(d *TargetDiag) {
			defer wg.Done()
			tctx, cancel := context.WithTimeout(ctx, o.Timeout)
			defer cancel()
			e.diagnoseOne(tctx, p, cfg, d, o)
		}(&out[i])
	}
	wg.Wait()
	return out, nil
}

func (e *Engine) diagnoseOne(ctx context.Context, p *product, cfg *config.Config, d *TargetDiag, o DiagnoseOptions) {
	s := cfg.System(d.Env, d.System)
	_, d.TypeKnown = connector.Lookup(s.Type)
	var viaPath string
	if len(s.Via) > 0 {
		chain, err := cfg.ViaChain(d.Env, d.System)
		var names []string
		for _, h := range chain {
			names = append(names, h.Name)
		}
		viaPath = strings.Join(names, ">")
		switch {
		case err != nil:
			d.Via = strings.Join(s.Via, ">") + ": config error"
		default:
			d.Via = viaPath + ": not checked"
			for _, h := range chain {
				if _, ok := connector.LookupTunneler(h.Type); !ok {
					d.Via = viaPath + ": no tunneler for " + h.Name + " (" + h.Type + ")"
				}
			}
		}
	}
	d.Login = "none"
	if s.Login != nil {
		d.Login, d.LoginCmd = "not checked", s.Login.Manual
		if s.Login.Manual == "browser" {
			d.LoginCmd = "tmvp login " + d.Env + " " + d.System
		}
	}
	if o.Offline {
		for _, n := range config.ReferencedSecrets(s) {
			st := "skipped"
			if cfg.LookupSecret(d.Env, n) == nil {
				st = "unknown"
			}
			d.Secrets = append(d.Secrets, SecretStatus{Name: n, State: st})
		}
		d.Connect = "skipped"
		return
	}
	d.Secrets = e.secretStates(ctx, p, cfg, d.Env, s, o.Timeout)
	if s.Login != nil && s.Login.Check != "" {
		if err := e.Login.Ensure(ctx, p.name+"/"+d.Env+"/"+d.System, loginSpec(s), cfg.Dir, d.Env, d.System); err != nil {
			d.Login = "required"
		} else {
			d.Login = "ok"
		}
	}
	if !d.TypeKnown || d.Login == "required" {
		d.Connect = "skipped"
		return
	}
	err := e.probe(ctx, p, cfg, d.Env, d.System)
	if err == nil {
		d.Connect = "ok"
		if viaPath != "" {
			d.Via = viaPath + ": ok"
		}
		return
	}
	ee := errs.As(err)
	ee.Message = e.Red.Redact(ee.Message)
	d.Connect, d.Error = "failed", ee
	if viaPath != "" && ee.System != "" && ee.System != d.System {
		d.Via = viaPath + ": failed at " + ee.System
	}
}
