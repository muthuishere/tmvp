// Package server holds the Engine (all request handling, keyed by product)
// and the unix-socket transport around it. The in-process client mode
// (TMVP_NO_SERVER=1) drives the same Engine directly.
package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/muthuishere/tmvp/internal/audit"
	"github.com/muthuishere/tmvp/internal/batch"
	"github.com/muthuishere/tmvp/internal/config"
	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
	"github.com/muthuishere/tmvp/internal/login"
	"github.com/muthuishere/tmvp/internal/protocol"
	"github.com/muthuishere/tmvp/internal/scan"
	"github.com/muthuishere/tmvp/internal/secret"
)

// Version is set by cmd/tmvp (ldflags) for status output.
var Version = "dev"

// Engine executes requests. Safe for concurrent use.
type Engine struct {
	Red    *secret.Redactor
	Login  *login.Checker
	Runner secret.Runner // nil = real processes

	started  time.Time
	active   atomic.Int64
	lastUse  atomic.Int64 // unix nano
	mu       sync.Mutex
	products map[string]*product
	stopFn   func()
}

// NewEngine creates an engine with a fresh redactor.
func NewEngine() *Engine {
	e := &Engine{Red: secret.NewRedactor(), Login: login.New(), started: time.Now(), products: map[string]*product{}}
	e.touch()
	return e
}

func (e *Engine) touch() { e.lastUse.Store(time.Now().UnixNano()) }

// Idle reports how long the engine has had no request in flight.
func (e *Engine) Idle() time.Duration {
	if e.active.Load() > 0 {
		return 0
	}
	return time.Since(time.Unix(0, e.lastUse.Load()))
}

// OnStop sets the func called by the server.stop op.
func (e *Engine) OnStop(f func()) { e.stopFn = f }

type product struct {
	name    string
	path    string // canonical config path; products are keyed by (path, name)
	mu      sync.Mutex
	cfg     *config.Config
	store   *secret.Store
	audit   *audit.Logger
	audPtr  atomic.Pointer[audit.Logger] // for secret events (no p.mu needed)
	conns   map[string]*conn
	tunnels map[string]connector.Tunnel
}

type conn struct {
	// refs/retired are guarded by product.mu: a conn replaced on reload or
	// auth refresh is closed only when its last in-flight call releases it.
	refs    int
	retired bool

	mu     sync.Mutex
	c      connector.Connector
	fp     string
	opened bool
	used   *usedSecrets
	since  time.Time
}

// Handle runs one request, streaming frames to sink, and returns the exit code.
// It does not emit the final "done" frame.
func (e *Engine) Handle(ctx context.Context, req protocol.Request, sink protocol.Sink) int {
	e.active.Add(1)
	defer func() { e.active.Add(-1); e.touch() }()
	fail := func(err error) int {
		ee := errs.As(err)
		ee.Message = e.Red.Redact(ee.Message)
		sink(protocol.Frame{Type: protocol.FrameError, Env: ee.Env, System: ee.System, Error: ee})
		return errs.ExitCode(ee.Code)
	}
	switch req.Op {
	case protocol.OpPing:
		sink(protocol.Frame{Type: protocol.FrameInfo, Info: map[string]any{"version": Version, "proto": protocol.Version}})
		return 0
	case protocol.OpStatus:
		sink(protocol.Frame{Type: protocol.FrameInfo, Info: e.status()})
		return 0
	case protocol.OpStop:
		sink(protocol.Frame{Type: protocol.FrameText, Text: "server stopping"})
		if e.stopFn != nil {
			go e.stopFn()
		}
		return 0
	}
	ctx = secret.WithTrigger(ctx, strings.TrimSpace(strings.Join([]string{req.Op, req.Env, req.System, req.Verb}, " ")))
	if req.EnvSource != "" {
		inner, src := sink, req.EnvSource
		sink = func(f protocol.Frame) { f.EnvSource = src; inner(f) }
	}
	if req.ConfigPath == "" {
		return fail(errs.New(errs.Config, "request has no config path"))
	}
	p, err := e.product(req.ConfigPath)
	if err != nil {
		return fail(err)
	}
	switch req.Op {
	case protocol.OpRun:
		return e.run(ctx, p, req, sink, false)
	case protocol.OpWrite:
		return e.run(ctx, p, req, sink, true)
	case protocol.OpSecretsStatus:
		return e.secretsStatus(ctx, p, req, sink)
	case protocol.OpCheck, protocol.OpLogin, protocol.OpSecretsReload, protocol.OpSecretsClear:
		var code int
		switch req.Op {
		case protocol.OpCheck:
			code = e.check(ctx, p, req, sink)
		case protocol.OpLogin:
			code = e.login(ctx, p, req, sink)
		default:
			code = e.secrets(ctx, p, req, sink)
		}
		var ee *errs.Error
		if code != 0 {
			ee = &errs.Error{Code: errs.Runtime, Message: fmt.Sprintf("exit %d", code)}
		}
		e.auditEvent(p.logger(), req, ee)
		return code
	}
	return fail(errs.New(errs.Usage, "unknown op %q", req.Op))
}

// product loads (or hot-reloads) the product behind configPath. Products
// are keyed by (canonical config path, product name): a second config that
// claims an already-loaded product name gets its own state (secrets, pools,
// audit) and never shares the first one's.
func (e *Engine) product(configPath string) (*product, error) {
	canon := canonicalPath(configPath)
	h, err := config.HashFile(configPath)
	if err != nil {
		return nil, errs.Wrap(errs.Config, err, "read config")
	}
	if p := e.findProduct(canon, h); p != nil {
		return p, nil
	}
	// config.Load may run for a while (big file, includes): never under e.mu
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	key := canon + "\x00" + cfg.Product
	p := e.products[key]
	if p == nil {
		p = &product{name: cfg.Product, path: canon, store: secret.NewStore(cfg.Product, e.Runner, e.Red),
			conns: map[string]*conn{}, tunnels: map[string]connector.Tunnel{}}
		pp := p
		p.store.OnEvent = func(ev secret.Event) { secretEvent(pp.audPtr.Load(), ev) }
		e.products[key] = p
	}
	// the same file now names a different product: drop the old product's state
	for k, op := range e.products {
		if op != p && op.path == canon {
			op.mu.Lock()
			op.closeAll()
			op.mu.Unlock()
			delete(e.products, k)
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cfg != nil && p.cfg.Hash == cfg.Hash {
		return p, nil // a concurrent loader got here first
	}
	old := p.cfg
	p.cfg = cfg
	p.audit = audit.New(cfg.Product, cfg.LogDir(), e.Red)
	p.audPtr.Store(p.audit)
	if old != nil {
		// hot reload: drop changed secrets, retire pools whose inputs changed
		p.store.Retain(secretDefs(cfg))
		for k, c := range p.conns {
			env, sys, _ := splitKey(k)
			// tunnels are all closed below, so a conn dialing through a via
			// chain must reopen too, even if its own inputs are unchanged
			viaChain := false
			if os := old.System(env, sys); os != nil && len(os.Via) > 0 {
				viaChain = true
			}
			if viaChain || cfg.Fingerprint(env, sys) != c.fp {
				p.retire(k, c)
			}
		}
		for k, t := range p.tunnels {
			_ = t.Close()
			delete(p.tunnels, k)
		}
	}
	return p, nil
}

func (e *Engine) findProduct(canon, hash string) *product {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, p := range e.products {
		p.mu.Lock()
		same := p.path == canon && p.cfg.Hash == hash
		p.mu.Unlock()
		if same {
			return p
		}
	}
	return nil
}

func canonicalPath(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		p = a
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return filepath.Clean(p)
}

// retire removes c from the pool; it is closed now if idle, else by the
// last release. Caller holds p.mu.
func (p *product) retire(key string, c *conn) {
	if p.conns[key] == c {
		delete(p.conns, key)
	}
	c.retired = true
	if c.refs == 0 {
		_ = c.c.Close()
	}
}

// release drops one in-flight reference taken by Engine.conn.
func (p *product) release(c *conn) {
	if c == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	c.refs--
	if c.retired && c.refs == 0 {
		_ = c.c.Close()
	}
}

// closeAll closes every conn and tunnel now (shutdown). Caller holds p.mu.
func (p *product) closeAll() {
	for k, c := range p.conns {
		c.retired = true
		_ = c.c.Close()
		delete(p.conns, k)
	}
	for k, t := range p.tunnels {
		_ = t.Close()
		delete(p.tunnels, k)
	}
}

func splitKey(k string) (env, sys, mode string) {
	parts := strings.SplitN(k, "/", 3)
	for len(parts) < 3 {
		parts = append(parts, "")
	}
	return parts[0], parts[1], parts[2]
}

func secretDefs(cfg *config.Config) []secret.Def {
	var out []secret.Def
	for _, d := range cfg.AllSecrets() {
		out = append(out, toSecretDef(cfg, d))
	}
	return out
}

func toSecretDef(cfg *config.Config, d *config.SecretDef) secret.Def {
	sd := secret.Def{Key: d.Key(), Name: d.Origin, File: d.File, Dir: cfg.Dir, Fingerprint: d.Fingerprint()}
	if sd.Name == "" {
		sd.Name = d.Name
	}
	if d.DotenvKey != "" {
		path := scan.ResolveDotenv(cfg.Dir, d.DotenvFile)
		sd.Fetch = func(context.Context) ([]byte, error) { return scan.DotenvValue(path, d.DotenvKey) }
		return sd
	}
	if d.Command == "" {
		sd.Fetch = func(context.Context) ([]byte, error) {
			v, err := cfg.ExprValue(d)
			return []byte(v), err
		}
		return sd
	}
	cmd, err := cfg.CommandText(d)
	if err != nil {
		sd.Fetch = func(context.Context) ([]byte, error) { return nil, err }
		return sd
	}
	sd.Command = cmd
	return sd
}

// snapshot returns the current config under lock.
func (p *product) config() *config.Config {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cfg
}

func (p *product) logger() *audit.Logger {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.audit
}

func (e *Engine) run(ctx context.Context, p *product, req protocol.Request, sink protocol.Sink, write bool) int {
	cfg, aud := p.config(), p.logger()
	fail := func(err error) int {
		ee := errs.As(err)
		sink(protocol.Frame{Type: protocol.FrameError, Env: ee.Env, System: ee.System, Error: ee})
		e.auditEvent(aud, req, ee)
		return errs.ExitCode(ee.Code)
	}
	ls, err := config.ParseLabels(req.Labels)
	if err != nil {
		return fail(err)
	}
	if write {
		if strings.TrimSpace(req.Reason) == "" {
			return fail(errs.New(errs.PolicyBlocked, "tmvp write requires --reason").WithHint(`tmvp write <env> <system> ... --reason "why"`))
		}
		if req.Env == "" || strings.ContainsAny(req.Env, ",*?[") || req.Env == "all" ||
			req.System == "" || strings.ContainsAny(req.System, "*?[") {
			return fail(errs.New(errs.PolicyBlocked, "writes run on exactly one env and one system").WithHint("tmvp write <env> <system> ..."))
		}
		if _, ok := cfg.Envs[req.Env]; ok && cfg.IsProtected(req.Env) && !req.Force && !req.Preview {
			src := req.EnvSource
			if src == "" {
				src = protocol.EnvExplicit
			}
			return fail(errs.New(errs.PolicyBlocked, "env %s is protected (destructive: false; env chosen by %s): writes need --force as well as --reason", req.Env, src).
				WithHint(fmt.Sprintf(`tmvp write %s %s ... --reason "why" --force   (try --preview first)`, req.Env, req.System)))
		}
	}
	targets, err := cfg.Select(config.Selector{Env: req.Env, System: req.System, Labels: ls})
	if err != nil {
		return fail(err)
	}
	since, _ := time.ParseDuration(req.Since)
	call := connector.Call{Verb: req.Verb, Args: req.Args, Flags: req.Flags, Reason: req.Reason,
		Preview: req.Preview, Limit: req.Limit, Since: since, Write: write}
	if call.Verb == "" {
		return fail(errs.New(errs.Usage, "no verb given"))
	}
	units := make([]batch.Unit, 0, len(targets))
	calls := map[string]connector.Call{}
	guards := map[string]*costGuard{}
	knownSomewhere := false
	for _, t := range targets {
		u := batch.Unit{Env: t.Env, System: t.Name}
		if t.System == nil {
			u.Skip = "env " + t.Env + " has no system " + t.Name
			units = append(units, u)
			continue
		}
		f, ok := connector.Lookup(t.System.Type)
		if !ok {
			u.Skip = "no connector for type " + t.System.Type
			units = append(units, u)
			continue
		}
		vs := f().Verbs()
		c := call
		v, found := connector.FindVerb(vs, c.Verb)
		if !write && !found && strings.HasPrefix(c.Verb, "@") {
			// bare saved item: `tmvp prod bastion @disk` → `script @disk`
			if sv, ok := connector.SavedVerb(vs); ok {
				c.Args = append([]string{c.Verb}, c.Args...)
				c.Verb, v, found = sv.Name, sv, true
			}
		}
		if write && (!found || !v.Mode.Writable()) {
			if dv, ok := connector.DefaultWriteVerb(vs); ok && !found {
				c.Args = append([]string{c.Verb}, c.Args...)
				c.Verb, v, found = dv.Name, dv, true
			} else if found {
				return fail(errs.New(errs.Usage, "%s is a read verb; run it without `write`", c.Verb).At(t.Env, t.Name))
			}
		}
		if !found {
			u.Skip = fmt.Sprintf("type %s has no verb %q", t.System.Type, call.Verb)
			units = append(units, u)
			continue
		}
		knownSomewhere = true
		if !write && !v.Mode.Readable() {
			return fail(errs.New(errs.PolicyBlocked, "%s is a %s verb; reads cannot mutate", v.Name, v.Mode).
				WithHint(fmt.Sprintf(`tmvp write <env> %s %s ... --reason "why"`, t.Name, v.Name)).At(t.Env, t.Name))
		}
		to, src, err := cfg.EffectiveTimeout(t.Env, t.Name, req.Timeout)
		if err != nil {
			return fail(err)
		}
		c.Timeout, u.Timeout = to, to
		guards[t.Env+"/"+t.Name] = &costGuard{force: req.Force, timeout: to, source: src, read: !write}
		c.Destructive = cfg.IsDestructive(t.Env, t.Name)
		if v.Mode == connector.Destructive && !c.Destructive {
			return fail(errs.New(errs.PolicyBlocked, "%s is destructive and %s.%s has destructive: false", v.Name, t.Env, t.Name).At(t.Env, t.Name))
		}
		if g, ok := f().(connector.Guard); ok {
			if err := g.Guard(c); err != nil {
				return fail(errs.As(err).At(t.Env, t.Name))
			}
		}
		calls[t.Env+"/"+t.Name] = c
		units = append(units, u)
	}
	if !knownSomewhere {
		var hint []string
		for _, t := range targets {
			if t.System != nil {
				for _, v := range connector.VerbsOf(t.System.Type) {
					hint = append(hint, v.Name)
				}
				break
			}
		}
		return fail(errs.New(errs.Usage, "verb %q not supported by the selected systems", call.Verb).WithHint("verbs: " + strings.Join(hint, ", ")))
	}
	mode := connector.ModeRead
	if write {
		mode = connector.ModeWrite
	}
	opts := batch.Options{Timeout: config.DefaultTimeout, Diff: req.Diff && !write, Write: write}
	if req.Limit > 0 {
		opts.Rows, opts.Lines = req.Limit, req.Limit
	}
	exec := func(ctx context.Context, u batch.Unit, em connector.Emit) error {
		c := calls[u.Env+"/"+u.System]
		start := time.Now()
		ce := &countEmit{Emit: em}
		attrs := map[string]any{"op": req.Op, "env": u.Env, "system": u.System, "verb": c.Verb, "mode": string(mode),
			"args": c.Args, "reason": req.Reason, "agent": req.Agent, "preview": c.Preview,
			"env_source": req.EnvSource, "env_selector": req.Env, "force": req.Force}
		if chain, err := cfg.ViaChain(u.Env, u.System); err == nil && len(chain) > 0 {
			hops := make([]string, len(chain))
			for i, h := range chain {
				hops[i] = h.Name
			}
			attrs["via"] = strings.Join(hops, ">")
		}
		var wid string
		if write {
			wid, _ = aud.WriteIntent(attrs)
		}
		ctx = secret.WithTrigger(ctx, strings.TrimSpace(fmt.Sprintf("%s %s.%s %s", req.Op, u.Env, u.System, c.Verb)))
		g := guards[u.Env+"/"+u.System]
		err := e.do(ctx, p, cfg, u.Env, u.System, mode, c, ce, g)
		g.audit(attrs)
		attrs["duration_ms"] = time.Since(start).Milliseconds()
		attrs["rows"], attrs["lines"] = ce.rows.Load(), ce.lines.Load()
		attrs["outcome"] = "ok"
		if err != nil {
			attrs["outcome"], attrs["code"] = "failed", string(errs.CodeOf(err))
			attrs["error"] = errs.As(err).Message
		}
		if write {
			_ = aud.WriteOutcome(wid, attrs)
		} else {
			_ = aud.Event("command", attrs)
		}
		return err
	}
	res := batch.Run(ctx, units, exec, opts, sink)
	return res.Exit
}

func (e *Engine) auditEvent(aud *audit.Logger, req protocol.Request, err *errs.Error) {
	if aud == nil {
		return
	}
	a := map[string]any{"op": req.Op, "env": req.Env, "system": req.System,
		"verb": req.Verb, "args": req.Args, "reason": req.Reason, "agent": req.Agent, "outcome": "ok"}
	if req.EnvSource != "" {
		a["env_source"] = req.EnvSource
	}
	if err != nil {
		a["outcome"], a["code"], a["error"] = "failed", string(err.Code), err.Message
	}
	_ = aud.Event("command", a)
}

type countEmit struct {
	connector.Emit
	rows, lines atomic.Int64
}

func (c *countEmit) Row(r connector.Row) error {
	err := c.Emit.Row(r)
	if err == nil {
		c.rows.Add(1)
	}
	return err
}

func (c *countEmit) Text(s string) error {
	err := c.Emit.Text(s)
	if err == nil {
		c.lines.Add(1)
	}
	return err
}

// do opens (or reuses) the connector and runs the call; on an auth error it
// re-runs the secrets the target used and retries once (DESIGN §8) — but
// only when the failure happened in Open or the call is a read: a write that
// reached the system is never run twice.
//
// For reads, g (the cost guard) estimates the call first and refuses it with
// too_expensive when the estimate exceeds the effective timeout.
func (e *Engine) do(ctx context.Context, p *product, cfg *config.Config, env, sys string, mode connector.Mode, c connector.Call, em connector.Emit, g *costGuard) error {
	cn, err := e.conn(ctx, p, cfg, env, sys, mode)
	openFailed := err != nil
	if err == nil {
		if gerr := g.check(ctx, cn.c, c, env, sys); gerr != nil {
			p.release(cn)
			return tag(gerr, env, sys)
		}
		err = cn.c.Do(ctx, c, em)
		if err == connector.ErrLimit {
			err = nil
		}
	}
	p.release(cn)
	if !errs.IsAuth(err) {
		return tag(err, env, sys)
	}
	// auth error: refresh used secrets (so the next call gets fresh ones)
	rerr := e.refreshAfterAuth(ctx, p, cfg, env, sys, mode, cn)
	if !openFailed && mode == connector.ModeWrite {
		ee := errs.As(err)
		if ee.Hint == "" {
			ee.Hint = "the write was not retried (it may have partly run); check the outcome, then run it again"
		}
		return tag(ee, env, sys)
	}
	if rerr != nil {
		return tag(rerr, env, sys)
	}
	cn, err = e.conn(ctx, p, cfg, env, sys, mode)
	if err != nil {
		p.release(cn)
		return tag(err, env, sys)
	}
	err = cn.c.Do(ctx, c, em)
	p.release(cn)
	if err == connector.ErrLimit {
		err = nil
	}
	if errs.IsAuth(err) {
		return tag(errs.New(errs.SecretInvalid, "credentials still rejected after re-fetching secrets").At(env, sys), env, sys)
	}
	return tag(err, env, sys)
}

func tag(err error, env, sys string) error {
	if err == nil {
		return nil
	}
	return errs.As(err).At(env, sys)
}

func (e *Engine) refreshAfterAuth(ctx context.Context, p *product, cfg *config.Config, env, sys string, mode connector.Mode, cn *conn) error {
	key := env + "/" + sys + "/" + string(mode)
	var used []secret.Def
	if cn != nil && cn.used != nil {
		used = cn.used.list()
	} else {
		s := cfg.System(env, sys)
		for _, n := range config.ReferencedSecrets(s) {
			if d := cfg.LookupSecret(env, n); d != nil {
				used = append(used, toSecretDef(cfg, d))
			}
		}
	}
	p.mu.Lock()
	if old := p.conns[key]; old != nil {
		p.retire(key, old)
	}
	p.mu.Unlock()
	e.Login.Forget(p.name + "/" + env + "/" + sys)
	if len(used) == 0 {
		return errs.New(errs.Auth, "authentication failed (no secrets to refresh)")
	}
	changed := false
	var lastErr error
	for _, d := range used {
		err := p.store.Invalidate(ctx, d)
		if err == nil {
			changed = true
		} else {
			lastErr = err
		}
	}
	if !changed {
		return lastErr
	}
	return nil
}

// conn returns an opened connector for env/sys/mode (warm pool) with one
// reference held: the caller must p.release it (also when err != nil and
// the returned conn is non-nil).
func (e *Engine) conn(ctx context.Context, p *product, cfg *config.Config, env, sys string, mode connector.Mode) (*conn, error) {
	key := env + "/" + sys + "/" + string(mode)
	s := cfg.System(env, sys)
	if s == nil {
		return nil, errs.New(errs.NotFound, "no system %s in env %s", sys, env)
	}
	f, ok := connector.Lookup(s.Type)
	if !ok {
		return nil, errs.New(errs.Unsupported, "no connector registered for type %q", s.Type)
	}
	p.mu.Lock()
	cn := p.conns[key]
	if cn == nil {
		cn = &conn{c: f(), fp: cfg.Fingerprint(env, sys), since: time.Now()}
		p.conns[key] = cn
	}
	cn.refs++
	p.mu.Unlock()
	cn.mu.Lock()
	opened := cn.opened
	cn.mu.Unlock()
	if !opened {
		// the login check may run an external command: not under cn.mu
		if err := e.Login.Ensure(ctx, p.name+"/"+env+"/"+sys, loginSpec(s), cfg.Dir, env, sys); err != nil {
			e.dropConn(p, key, cn)
			p.release(cn)
			return nil, err
		}
	}
	cn.mu.Lock()
	defer cn.mu.Unlock()
	if cn.opened {
		return cn, nil
	}
	t, used, err := e.target(ctx, p, cfg, s, mode)
	if err != nil {
		e.dropConn(p, key, cn)
		p.release(cn)
		return nil, err
	}
	cn.used = used
	octx, cancel := context.WithTimeout(ctx, t.Limits.Timeout)
	defer cancel()
	if err := cn.c.Open(octx, t); err != nil {
		_ = cn.c.Close()
		e.dropConn(p, key, cn)
		return cn, err // cn returned so auth retry knows which secrets were used
	}
	cn.opened = true
	return cn, nil
}

func (e *Engine) dropConn(p *product, key string, cn *conn) {
	p.mu.Lock()
	if p.conns[key] == cn {
		delete(p.conns, key)
	}
	p.mu.Unlock()
}

func loginSpec(s *config.System) *login.Spec {
	if s.Login == nil {
		return nil
	}
	return &login.Spec{Manual: s.Login.Manual, Check: s.Login.Check}
}

// target builds the connector.Target for s, opening via tunnels as needed.
func (e *Engine) target(ctx context.Context, p *product, cfg *config.Config, s *config.System, mode connector.Mode) (*connector.Target, *usedSecrets, error) {
	used := &usedSecrets{m: map[string]secret.Def{}}
	f := &fields{cfg: cfg, env: s.Env, sys: s, store: p.store, used: used}
	dialer := connector.DirectDialer()
	if len(s.Via) > 0 {
		chain, err := cfg.ViaChain(s.Env, s.Name)
		if err != nil {
			return nil, nil, errs.Wrap(errs.Config, err, "via")
		}
		var path []string
		for _, hop := range chain {
			path = append(path, hop.Name)
			tk := s.Env + "/" + strings.Join(path, ">")
			p.mu.Lock()
			tun := p.tunnels[tk]
			p.mu.Unlock()
			if tun == nil {
				tn, ok := connector.LookupTunneler(hop.Type)
				if !ok {
					return nil, nil, errs.New(errs.Unsupported, "via %s: no tunneler registered for type %q", hop.Name, hop.Type)
				}
				if err := e.Login.Ensure(ctx, p.name+"/"+s.Env+"/"+hop.Name, loginSpec(hop), cfg.Dir, s.Env, hop.Name); err != nil {
					return nil, nil, err
				}
				hf := &fields{cfg: cfg, env: s.Env, sys: hop, store: p.store, used: used}
				ht := connector.NewTarget(e.baseTarget(p, cfg, hop, connector.ModeRead, dialer), hf)
				t, err := tn.OpenTunnel(ctx, ht, dialer)
				if err != nil {
					return nil, nil, errs.As(err).At(s.Env, hop.Name)
				}
				p.mu.Lock()
				if existing := p.tunnels[tk]; existing != nil {
					_ = t.Close()
					t = existing
				} else {
					p.tunnels[tk] = t
				}
				p.mu.Unlock()
				tun = t
			}
			dialer = tun
		}
	}
	return connector.NewTarget(e.baseTarget(p, cfg, s, mode, dialer), f), used, nil
}

func (e *Engine) baseTarget(p *product, cfg *config.Config, s *config.System, mode connector.Mode, d connector.Dialer) connector.Target {
	return connector.Target{Product: p.name, Env: s.Env, System: s.Name, Type: s.Type, Mode: mode,
		Destructive: cfg.IsDestructive(s.Env, s.Name), Labels: s.Labels, Via: s.Via,
		Login: targetLogin(s), Redactor: e.Red, Limits: limitsOf(cfg, s), Dir: cfg.Dir, AssetRoots: cfg.AssetRoots(), Dialer: d}
}

func targetLogin(s *config.System) *connector.LoginSpec {
	if s.Login == nil {
		return nil
	}
	return &connector.LoginSpec{Manual: s.Login.Manual, Check: s.Login.Check}
}

// limitsOf merges global < env < system `limits:` into connector.Limits.
// Timeout follows the config precedence (system > env > global > 10s).
func limitsOf(cfg *config.Config, s *config.System) connector.Limits {
	l := connector.DefaultLimits
	m := cfg.MergedLimits(s.Env, s.Name)
	geti := func(k string, dst *int) {
		switch v := m[k].(type) {
		case int:
			*dst = v
		case float64:
			*dst = int(v)
		}
	}
	getf := func(k string, dst *float64) {
		if v, ok := m[k]; ok {
			if f, err := config.ParseLimitFloat(v); err == nil {
				*dst = f
			}
		}
	}
	geti("rows", &l.Rows)
	geti("lines", &l.Lines)
	geti("bytes", &l.Bytes)
	l.Timeout, _ = cfg.ConfigTimeout(s.Env, s.Name)
	if v, ok := m["statement_timeout"]; ok {
		if d, err := config.ParseLimitDuration(v); err == nil {
			l.StatementTimeout = d
		}
	}
	getf("cost_per_second", &l.CostPerSecond)
	getf("rows_per_second", &l.RowsPerSecond)
	getf("bytes_per_second", &l.BytesPerSecond)
	getf("points_per_second", &l.PointsPerSecond)
	getf("seq_scan_rows", &l.SeqScanRows)
	return l
}

// Shutdown closes every connection and tunnel and drops all secrets.
func (e *Engine) Shutdown() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, p := range e.products {
		p.mu.Lock()
		p.closeAll()
		p.store.Clear()
		p.mu.Unlock()
	}
}

func (e *Engine) status() map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	var prods []map[string]any
	for _, p := range e.products {
		p.mu.Lock()
		var conns, tuns []string
		for k, c := range p.conns {
			if c.opened {
				conns = append(conns, k)
			}
		}
		for k := range p.tunnels {
			tuns = append(tuns, k)
		}
		prods = append(prods, map[string]any{"product": p.name, "config": p.cfg.Path,
			"connections": conns, "tunnels": tuns, "secrets": p.store.Status()})
		p.mu.Unlock()
	}
	return map[string]any{"version": Version, "pid": os.Getpid(), "uptime": time.Since(e.started).Round(time.Second).String(),
		"idle": e.Idle().Round(time.Second).String(), "products": prods, "connectors": connector.Types(),
		"tunnelers": connector.TunnelerTypes()}
}
