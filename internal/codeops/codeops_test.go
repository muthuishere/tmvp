package codeops

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/muthuishere/clicodemode/codemode"

	"github.com/muthuishere/tmvp/internal/config"
	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
	"github.com/muthuishere/tmvp/internal/protocol"
)

type typeA struct{}
type typeB struct{}

func init() {
	connector.Register("cotesta", func() connector.Connector { return typeA{} })
	connector.Register("cotestb", func() connector.Connector { return typeB{} })
}

func (typeA) Type() string { return "cotesta" }
func (typeA) Verbs() []connector.Verb {
	return []connector.Verb{{Name: "q", Mode: connector.Read}, {Name: "w", Mode: connector.Write}, {Name: "rw", Mode: connector.ReadWrite}, {Name: "Bad", Mode: connector.Read}}
}
func (typeA) Open(context.Context, *connector.Target) error            { return nil }
func (typeA) Do(context.Context, connector.Call, connector.Emit) error { return nil }
func (typeA) Close() error                                             { return nil }
func (typeB) Type() string                                             { return "cotestb" }
func (typeB) Verbs() []connector.Verb                                  { return []connector.Verb{{Name: "tail", Mode: connector.Read}} }
func (typeB) Open(context.Context, *connector.Target) error            { return nil }
func (typeB) Do(context.Context, connector.Call, connector.Emit) error { return nil }
func (typeB) Close() error                                             { return nil }

func load(t *testing.T, y string) *config.Config {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(y), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

const twoTypes = `product: co
envs:
  dev:
    systems:
      db: { type: cotesta }
      MixedCase: { type: cotesta }
  prod:
    systems:
      db: { type: cotestb }
`

func names(reg *codemode.Registry) string {
	var out []string
	for _, op := range reg.Operations() {
		out = append(out, op.Name+"="+string(op.SideEffects))
	}
	return strings.Join(out, " ")
}

func TestBuildRegistry(t *testing.T) {
	cfg := load(t, twoTypes)
	reg, skipped, err := Build(Options{Cfg: cfg, Send: nil, Version: "t"})
	if err != nil {
		t.Fatal(err)
	}
	got := names(reg)
	want := "db.q=none db.rw=none db.tail=none write.db.rw=external write.db.w=external"
	if got != want {
		t.Fatalf("ops:\n got %s\nwant %s", got, want)
	}
	if strings.Join(skipped, ",") != "db.Bad,MixedCase" {
		t.Fatalf("skipped: %v", skipped)
	}
	op, _ := reg.Lookup("db.q")
	var s map[string]any
	_ = json.Unmarshal(op.InputSchema, &s)
	if req, _ := s["required"].([]any); len(req) != 1 || req[0] != "env" {
		t.Fatalf("env must be required without default_env: %s", op.InputSchema)
	}
	if s["additionalProperties"] != false {
		t.Fatal("unknown args must be rejected")
	}
	w, _ := reg.Lookup("write.db.w")
	_ = json.Unmarshal(w.InputSchema, &s)
	if r := s["required"].([]any); len(r) != 2 || r[1] != "reason" {
		t.Fatalf("write schema: %s", w.InputSchema)
	}
}

func TestEnvOptionalWithDefault(t *testing.T) {
	cfg := load(t, "product: co\ndefault_env: dev\nenvs:\n  dev:\n    systems:\n      db: { type: cotesta }\n")
	reg, _, _ := Build(Options{Cfg: cfg, Version: "t"})
	op, _ := reg.Lookup("db.q")
	if strings.Contains(string(op.InputSchema), `"required"`) {
		t.Fatalf("env should be optional with default_env: %s", op.InputSchema)
	}
	var got protocol.Request
	reg2, _, _ := Build(Options{Cfg: cfg, Version: "t", Send: func(_ context.Context, r protocol.Request, _ func(protocol.Frame)) (int, error) {
		got = r
		return 0, nil
	}})
	op, _ = reg2.Lookup("db.q")
	t.Setenv("TMVP_HOME", t.TempDir())
	t.Setenv("TMVP_ENV", "")
	if _, err := op.Handler(context.Background(), map[string]any{"args": []any{"a"}, "limit": float64(5)}); err != nil {
		t.Fatal(err)
	}
	if got.Op != protocol.OpRun || got.Env != "dev" || got.EnvSource != protocol.EnvDefault || got.Limit != 5 || got.Force || got.Args[0] != "a" {
		t.Fatalf("request: %+v", got)
	}
}

func TestSendThrowsOnlyWhenNothingSucceeded(t *testing.T) {
	partial := func(_ context.Context, _ protocol.Request, fn func(protocol.Frame)) (int, error) {
		fn(protocol.Frame{Type: protocol.FrameRow, Env: "dev", System: "db", Row: map[string]any{"n": 1}})
		fn(protocol.Frame{Type: protocol.FrameStatus, Env: "dev", System: "db", Status: "ok"})
		fn(protocol.Frame{Type: protocol.FrameError, Error: errs.New(errs.TooExpensive, "prod.db est. ~1h").At("prod", "db")})
		fn(protocol.Frame{Type: protocol.FrameStatus, Env: "prod", System: "db", Status: "failed"})
		return 4, nil
	}
	v, err := Send(context.Background(), partial, protocol.Request{})
	if err != nil {
		t.Fatal(err)
	}
	r := v.(Result)
	if len(r.Rows) != 1 || r.Rows[0]["env"] != "dev" || len(r.Errors) != 1 || r.Errors[0].Code != "too_expensive" || r.Exit != 4 {
		t.Fatalf("%+v", r)
	}
	refused := func(_ context.Context, _ protocol.Request, fn func(protocol.Frame)) (int, error) {
		fn(protocol.Frame{Type: protocol.FrameError, Error: errs.New(errs.PolicyBlocked, "mut is a write verb").WithHint("tmvp write")})
		return 3, nil
	}
	_, err = Send(context.Background(), refused, protocol.Request{})
	if err == nil || err.Error() != "policy_blocked: mut is a write verb (hint: tmvp write)" {
		t.Fatalf("got %v", err)
	}
}

func TestPolicy(t *testing.T) {
	ro := Policy(false)
	if ro(context.Background(), &codemode.Operation{Name: "db.q", SideEffects: codemode.SideEffectNone}) != nil {
		t.Fatal("read refused")
	}
	w := &codemode.Operation{Name: "write.db.w", SideEffects: codemode.SideEffectExternal}
	if err := ro(context.Background(), w); err == nil || !strings.HasPrefix(err.Error(), "policy_blocked") {
		t.Fatalf("write allowed without --allow-writes: %v", err)
	}
	if Policy(true)(context.Background(), w) != nil {
		t.Fatal("write refused with --allow-writes")
	}
	if Policy(true)(context.Background(), &codemode.Operation{Name: "x.exec", SideEffects: codemode.SideEffectLocal}) == nil {
		t.Fatal("non-write side effect allowed")
	}
}

func TestResolveScript(t *testing.T) {
	cfg := load(t, twoTypes)
	dir := filepath.Join(cfg.Dir, "scripts")
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "stuck.js"), []byte("// find stuck orders\nexport default 1\n"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "plain.js"), []byte("export default 2\n"), 0o600)
	if p, err := ResolveScript(cfg, "stuck"); err != nil || p != filepath.Join(dir, "stuck.js") {
		t.Fatalf("%q %v", p, err)
	}
	for _, a := range []string{"-", "./x.js", "x.js", "sub/x"} {
		if p, _ := ResolveScript(cfg, a); p != a {
			t.Errorf("%s resolved to %s", a, p)
		}
	}
	_, err := ResolveScript(cfg, "nope")
	if e := errs.As(err); e.Code != errs.NotFound || !strings.Contains(e.Hint, "plain, stuck") {
		t.Fatalf("%v", err)
	}
	ss := Scripts(cfg)
	if len(ss) != 2 || ss[1].Name != "stuck" || ss[1].Description != "find stuck orders" || ss[0].Description != "" {
		t.Fatalf("%+v", ss)
	}
}
