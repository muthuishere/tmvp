package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muthuishere/tmvp/internal/audit"
	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
	"github.com/muthuishere/tmvp/internal/protocol"
)

// costly is a fake connector with an estimator. Args: [estimate, sleep].
// estimate: a duration, "none" (ErrNoEstimate) or "fail" (estimator error).
// Do sleeps for `sleep` (honouring ctx) and reports the call's timeout.
type costly struct {
	t   *connector.Target
	dsn string
}

func init() { connector.Register("costly", func() connector.Connector { return &costly{} }) }

func (c *costly) Type() string { return "costly" }
func (c *costly) Verbs() []connector.Verb {
	return []connector.Verb{{Name: "q", Mode: connector.Read}, {Name: "w", Mode: connector.Write, Default: true}}
}
func (c *costly) Open(ctx context.Context, t *connector.Target) error {
	c.t = t
	d, err := t.ModeString(ctx, "dsn")
	c.dsn = d
	return err
}
func (c *costly) Close() error { return nil }
func (c *costly) Estimate(ctx context.Context, call connector.Call) (connector.Estimate, error) {
	if len(call.Args) == 0 {
		return connector.Estimate{}, connector.ErrNoEstimate
	}
	switch call.Args[0] {
	case "none":
		return connector.Estimate{}, connector.ErrNoEstimate
	case "fail":
		return connector.Estimate{}, errs.New(errs.Runtime, "explain failed for %s", c.dsn)
	}
	d, err := time.ParseDuration(call.Args[0])
	if err != nil {
		return connector.Estimate{}, err
	}
	// the Why deliberately carries the secret: the redactor must catch it
	return connector.Estimate{Duration: d, Rows: 4200000, Why: "Seq Scan on events, ~4.2M rows " + c.dsn, Confident: true}, nil
}
func (c *costly) Do(ctx context.Context, call connector.Call, e connector.Emit) error {
	sleep := time.Duration(0)
	if len(call.Args) > 1 {
		sleep, _ = time.ParseDuration(call.Args[1])
	}
	select {
	case <-time.After(sleep):
	case <-ctx.Done():
		return ctx.Err()
	}
	return e.Row(connector.Row{"timeout": call.Timeout.String(), "dsn": c.dsn})
}

const costYAML = `
version: 1
product: costp
logging: { path: ./logs }
limits: { timeout: 30s }
envs:
  dev:
    destructive: true
    limits: { timeout: 3s }
    secrets: { dsn: "$(get dev)" }
    systems:
      db:  { type: costly, dsn: "${secrets.dsn}", limits: { timeout: 2s } }
      raw: { type: costly, dsn: "${secrets.dsn}" }
  stage:
    destructive: true
    secrets: { dsn: "$(get dev)" }
    systems:
      db: { type: costly, dsn: "${secrets.dsn}" }
`

func costSetup(t *testing.T) (*Engine, string) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(costYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newSecrets()
	s.set("get dev", "FAKE_SECRET_123")
	return newEngine(s), p
}

func costAudit(t *testing.T, p string) []map[string]any {
	t.Helper()
	recs, err := audit.Read(filepath.Join(filepath.Dir(p), "logs"), audit.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, r := range recs {
		if r.Attributes["op"] == protocol.OpRun {
			out = append(out, r.Attributes)
		}
	}
	return out
}

func noSecret(t *testing.T, s string) {
	t.Helper()
	if strings.Contains(s, "FAKE_SECRET_123") {
		t.Fatalf("secret leaked: %s", s)
	}
}

func TestCostGuardBlocksOverLimit(t *testing.T) {
	e, p := costSetup(t)
	r := call(t, e, protocol.Request{Op: protocol.OpRun, ConfigPath: p, Env: "dev", System: "db", Verb: "q", Args: []string{"42s", "0s"}})
	noSecret(t, r.raw)
	if r.exit != 3 || len(r.of(protocol.FrameRow)) != 0 {
		t.Fatalf("want exit 3 and no rows: %s", r.raw)
	}
	ef := r.of(protocol.FrameError)
	if len(ef) != 1 || ef[0].Error.Code != errs.TooExpensive {
		t.Fatalf("frames: %s", r.raw)
	}
	m, h := ef[0].Error.Message, ef[0].Error.Hint
	if !strings.HasPrefix(m, "dev.db est. ~42.0s (Seq Scan on events, ~4.2M rows") || !strings.HasSuffix(m, ") > timeout 2s") {
		t.Fatalf("message: %q", m)
	}
	if h != "narrow the query, or --timeout 63s, or --force" {
		t.Fatalf("hint: %q", h)
	}
	a := costAudit(t, p)
	if len(a) != 1 || a[0]["blocked"] != true || a[0]["timeout_source"] != "system" || a[0]["code"] != "too_expensive" {
		t.Fatalf("audit: %v", a)
	}
	est, _ := a[0]["estimate"].(map[string]any)
	if est == nil || est["duration_ms"] != float64(42000) || est["rows"] != float64(4200000) {
		t.Fatalf("audit estimate: %v", a[0]["estimate"])
	}
	b, _ := os.ReadFile(filepath.Join(filepath.Dir(p), "logs", audit.FileName))
	noSecret(t, string(b))
}

func TestCostGuardTimeoutFlagRaisesAndRuns(t *testing.T) {
	e, p := costSetup(t)
	r := call(t, e, protocol.Request{Op: protocol.OpRun, ConfigPath: p, Env: "dev", System: "db", Verb: "q", Args: []string{"42s", "0s"}, Timeout: "1m"})
	noSecret(t, r.raw)
	rows := r.of(protocol.FrameRow)
	if r.exit != 0 || len(rows) != 1 || rows[0].Row["timeout"] != "1m0s" {
		t.Fatalf("want run with 1m: %s", r.raw)
	}
	a := costAudit(t, p)
	if a[0]["blocked"] != false || a[0]["timeout_source"] != "flag" || a[0]["timeout_ms"] != float64(60000) {
		t.Fatalf("audit: %v", a)
	}
	// and lowers it
	r = call(t, e, protocol.Request{Op: protocol.OpRun, ConfigPath: p, Env: "dev", System: "db", Verb: "q", Args: []string{"1500ms", "0s"}, Timeout: "1s"})
	if r.exit != 3 {
		t.Fatalf("lowered timeout must block: %s", r.raw)
	}
	if r := call(t, e, protocol.Request{Op: protocol.OpRun, ConfigPath: p, Env: "dev", System: "db", Verb: "q", Timeout: "soon"}); r.exit != 2 {
		t.Fatalf("bad --timeout is a usage error: %s", r.raw)
	}
}

func TestCostGuardForceRunsButHardTimeoutKills(t *testing.T) {
	e, p := costSetup(t)
	// forced past a 42s estimate; still killed by the 300ms hard timeout
	start := time.Now()
	r := call(t, e, protocol.Request{Op: protocol.OpRun, ConfigPath: p, Env: "dev", System: "db", Verb: "q", Args: []string{"42s", "5s"}, Timeout: "300ms", Force: true})
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("hard timeout did not kill (%s)", el)
	}
	ef := r.of(protocol.FrameError)
	if r.exit != 1 || len(ef) != 1 || ef[0].Error.Code != errs.Timeout {
		t.Fatalf("want timeout: %s", r.raw)
	}
	a := costAudit(t, p)
	if a[0]["force"] != true || a[0]["blocked"] != false || a[0]["estimate"] != "skipped (--force)" {
		t.Fatalf("audit: %v", a)
	}
	// forced and fast: runs
	r = call(t, e, protocol.Request{Op: protocol.OpRun, ConfigPath: p, Env: "dev", System: "db", Verb: "q", Args: []string{"42s", "0s"}, Force: true})
	if r.exit != 0 || len(r.of(protocol.FrameRow)) != 1 {
		t.Fatalf("force: %s", r.raw)
	}
	noSecret(t, r.raw)
}

func TestCostGuardBatchMixed(t *testing.T) {
	e, p := costSetup(t)
	// 5s estimate: dev.db (2s, system) blocks, stage.db (30s, global) runs
	r := call(t, e, protocol.Request{Op: protocol.OpRun, ConfigPath: p, Env: "all", System: "db", Verb: "q", Args: []string{"5s", "0s"}})
	noSecret(t, r.raw)
	if r.exit != errs.ExitCode(errs.Partial) {
		t.Fatalf("want partial: %s", r.raw)
	}
	ef, rows := r.of(protocol.FrameError), r.of(protocol.FrameRow)
	if len(ef) != 1 || ef[0].Env != "dev" || ef[0].Error.Code != errs.TooExpensive || len(rows) != 1 || rows[0].Env != "stage" || rows[0].Row["timeout"] != "30s" {
		t.Fatalf("frames: %s", r.raw)
	}
	src := map[string]any{}
	for _, a := range costAudit(t, p) {
		src[fmt.Sprint(a["env"], ".", a["system"])] = a["timeout_source"]
	}
	if src["dev.db"] != "system" || src["stage.db"] != "global" {
		t.Fatalf("sources: %v", src)
	}
	// env level
	r = call(t, e, protocol.Request{Op: protocol.OpRun, ConfigPath: p, Env: "dev", System: "raw", Verb: "q", Args: []string{"2500ms", "0s"}})
	if r.exit != 0 || r.of(protocol.FrameRow)[0].Row["timeout"] != "3s" {
		t.Fatalf("env timeout: %s", r.raw)
	}
}

func TestCostGuardUnavailableRunsWithTimeout(t *testing.T) {
	e, p := costSetup(t)
	for _, arg := range []string{"none", "fail"} {
		r := call(t, e, protocol.Request{Op: protocol.OpRun, ConfigPath: p, Env: "dev", System: "db", Verb: "q", Args: []string{arg, "0s"}})
		if r.exit != 0 {
			t.Fatalf("%s: %s", arg, r.raw)
		}
	}
	a := costAudit(t, p)
	if len(a) != 2 || !strings.HasPrefix(fmt.Sprint(a[0]["estimate"]), "unavailable") || !strings.HasPrefix(fmt.Sprint(a[1]["estimate"]), "unavailable") {
		t.Fatalf("audit: %v", a)
	}
	b, _ := os.ReadFile(filepath.Join(filepath.Dir(p), "logs", audit.FileName))
	noSecret(t, string(b))
}

func TestCostGuardSkipsWrites(t *testing.T) {
	e, p := costSetup(t)
	r := call(t, e, protocol.Request{Op: protocol.OpWrite, ConfigPath: p, Env: "dev", System: "db", Verb: "w", Args: []string{"42s"}, Reason: "test"})
	if r.exit != 0 {
		t.Fatalf("writes are not estimated: %s", r.raw)
	}
}

func TestFmtDurAndSuggest(t *testing.T) {
	for d, want := range map[time.Duration]string{850 * time.Millisecond: "850ms", 4200 * time.Millisecond: "4.2s", 190 * time.Second: "3m10s"} {
		if got := fmtDur(d); got != want {
			t.Errorf("fmtDur(%s)=%s", d, got)
		}
	}
	if suggest(42*time.Second) != "63s" || suggest(10*time.Minute) != "15m" {
		t.Fatal(suggest(42*time.Second), suggest(10*time.Minute))
	}
}
