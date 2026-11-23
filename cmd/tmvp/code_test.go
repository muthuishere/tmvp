package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/muthuishere/tmvp/internal/audit"
	"github.com/muthuishere/tmvp/internal/client"
	"github.com/muthuishere/tmvp/internal/codeops"
	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/protocol"
)

// cmfake: q (read; arg "big" estimates 1h, "slow" sleeps), mut (write).
// It counts Do calls per verb so tests prove the server refused before Do.
type cmfake struct{}

var (
	cmMu sync.Mutex
	cmDo = map[string]int{}
	// cmSlow is closed-over per call: each "slow" call signals here when it
	// returns, so a test can wait for it before its temp dirs are removed.
	cmSlow = make(chan struct{}, 16)
)

// testOnlyTypes are connector types registered by tests only (no docs, no ls).
var testOnlyTypes = map[string]bool{"cmfake": true}

func init() { connector.Register("cmfake", func() connector.Connector { return &cmfake{} }) }

func (cmfake) Type() string { return "cmfake" }
func (cmfake) Verbs() []connector.Verb {
	return []connector.Verb{{Name: "q", Mode: connector.Read, Help: "q <arg>"},
		{Name: "mut", Mode: connector.Write, Default: true, Help: "mut <stmt>"}}
}
func (cmfake) Open(context.Context, *connector.Target) error { return nil }
func (cmfake) Close() error                                  { return nil }
func (cmfake) Estimate(_ context.Context, c connector.Call) (connector.Estimate, error) {
	if len(c.Args) > 0 && c.Args[0] == "big" {
		return connector.Estimate{Duration: time.Hour, Why: "Seq Scan", Confident: true}, nil
	}
	return connector.Estimate{}, connector.ErrNoEstimate
}
func (cmfake) Do(ctx context.Context, c connector.Call, e connector.Emit) error {
	cmMu.Lock()
	cmDo[c.Verb]++
	cmMu.Unlock()
	if len(c.Args) > 0 && c.Args[0] == "slow" {
		defer func() { cmSlow <- struct{}{} }()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	return e.Row(connector.Row{"arg": strings.Join(c.Args, " "), "write": c.Write})
}

func cmDoCount(verb string) int {
	cmMu.Lock()
	defer cmMu.Unlock()
	return cmDo[verb]
}

const codeCfg = `product: codetest
default_env: dev
envs:
  dev:
    destructive: true
    systems:
      db: { type: cmfake }
  staging:
    systems:
      db: { type: cmfake }
  prod:
    type: prod
    systems:
      db: { type: cmfake }
`

type envelope struct {
	OK    bool            `json:"ok"`
	Value json.RawMessage `json:"value"`
	Error string          `json:"error"`
}

// codeRun writes prog, runs `tmvp code run <file> --json [extra...]`.
func codeRun(t *testing.T, out *bytes.Buffer, prog string, extra ...string) (int, envelope) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "prog.js")
	if err := os.WriteFile(p, []byte(prog), 0o600); err != nil {
		t.Fatal(err)
	}
	code := run(t, out, append([]string{"code", "run", p, "--json"}, extra...)...)
	var env envelope
	_ = json.Unmarshal(out.Bytes(), &env)
	return code, env
}

func codeAudit(t *testing.T) []audit.Record {
	t.Helper()
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	recs, err := audit.Read(cfg.LogDir(), audit.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

func TestCodeRunParallelReadsAuditedPerCall(t *testing.T) {
	out := envSetup(t, codeCfg)
	code, env := codeRun(t, out, `
const rs = await Promise.all(["dev","staging","prod"].map(env => ops.db.q({ env, args: ["x-" + env] })))
export default rs.map(r => r.rows[0].env + ":" + r.rows[0].arg)`)
	if code != 0 || !env.OK || compact(env.Value) != `["dev:x-dev","staging:x-staging","prod:x-prod"]` {
		t.Fatalf("code %d: %s", code, out)
	}
	n := 0
	for _, r := range codeAudit(t) {
		if r.Attributes["op"] == protocol.OpRun && r.Attributes["verb"] == "q" {
			n++
		}
	}
	if n != 3 {
		t.Fatalf("want 3 audited calls, got %d", n)
	}
}

func TestCodeRunEnvSelectorAll(t *testing.T) {
	out := envSetup(t, codeCfg)
	code, env := codeRun(t, out, `const r = await ops.db.q({ env: "all", args: ["a"] }); export default r.rows.length`)
	if code != 0 || string(env.Value) != "3" {
		t.Fatalf("code %d: %s", code, out)
	}
}

func TestCodeRunWriteVerbThroughReadPathIsPolicyBlocked(t *testing.T) {
	out := envSetup(t, codeCfg)
	cfg, _ := loadConfig()
	eng, err := codeEngine(cfg, codeFlags{maxSteps: 1e6, maxTime: 10 * time.Second, concurrency: 4})
	if err != nil {
		t.Fatal(err)
	}
	// not exposed as a read op at all
	if _, ok := eng.Registry.Lookup("db.mut"); ok {
		t.Fatal("write verb registered as a read op")
	}
	_ = out
	before := cmDoCount("mut")
	// a read request for a write verb through the same client -> server path
	// the handlers use: the server refuses before the connector runs
	res, err := codeops.Send(context.Background(), client.Do, protocol.Request{Op: protocol.OpRun,
		ConfigPath: cfg.Path, Product: cfg.Product, Env: "dev", System: "db", Verb: "mut", Args: []string{"x"}})
	if err == nil || !strings.HasPrefix(err.Error(), "policy_blocked:") {
		t.Fatalf("want policy_blocked, got %v %v", res, err)
	}
	if cmDoCount("mut") != before {
		t.Fatal("connector Do ran for a refused write")
	}
}

func TestCodeRunWriteOpNeedsAllowWrites(t *testing.T) {
	out := envSetup(t, codeCfg)
	before := cmDoCount("mut")
	code, env := codeRun(t, out, `
let m = "ran"
try { await ops.write.db.mut({ env: "dev", args: ["x"], reason: "t" }) } catch (e) { m = e.message }
export default m`)
	_ = code
	if !strings.Contains(string(env.Value)+env.Error, "policy_blocked") {
		t.Fatalf("want policy_blocked: %s", out)
	}
	if cmDoCount("mut") != before {
		t.Fatal("write ran without --allow-writes")
	}
}

func TestCodeRunProdWriteWithoutForceRefusedInsideProgram(t *testing.T) {
	out := envSetup(t, codeCfg)
	before := cmDoCount("mut")
	code, env := codeRun(t, out, `
const out = {}
try { await ops.write.db.mut({ env: "prod", args: ["x"], reason: "fix #1" }); out.noforce = "ran" } catch (e) { out.noforce = e.message }
const noReason = JSON.parse('{"env":"prod","args":["x"]}')
try { await call("write.db.mut", noReason); out.noreason = "ran" } catch (e) { out.noreason = e.message }
try { await ops.write.db.mut({ env: "all", args: ["x"], reason: "r", force: true }); out.multi = "ran" } catch (e) { out.multi = e.message }
export default out`, "--allow-writes")
	if code != 0 {
		t.Fatalf("code %d: %s", code, out)
	}
	var v map[string]string
	_ = json.Unmarshal(env.Value, &v)
	if !strings.Contains(v["noforce"], "policy_blocked: env prod is protected") {
		t.Errorf("noforce: %q", v["noforce"])
	}
	if !strings.Contains(v["noreason"], "reason") {
		t.Errorf("noreason: %q", v["noreason"])
	}
	if !strings.Contains(v["multi"], "policy_blocked: writes run on exactly one env") {
		t.Errorf("multi: %q", v["multi"])
	}
	if cmDoCount("mut") != before {
		t.Fatal("a refused write reached the connector")
	}
	// allowed: dev is destructive, reason given; audited as a write
	code, env = codeRun(t, out, `const r = await ops.write.db.mut({ env: "dev", args: ["ok"], reason: "fix #2" }); export default r.rows[0].write`, "--allow-writes")
	if code != 0 || string(env.Value) != "true" {
		t.Fatalf("dev write: %d %s", code, out)
	}
	found := false
	for _, r := range codeAudit(t) {
		if r.Attributes["op"] == protocol.OpWrite && r.Attributes["reason"] == "fix #2" {
			found = true
		}
	}
	if !found {
		t.Fatal("write not audited")
	}
}

func TestCodeRunTooExpensiveIsCatchable(t *testing.T) {
	out := envSetup(t, codeCfg)
	before := cmDoCount("q")
	code, env := codeRun(t, out, `
let m = "ran"
try { await ops.db.q({ env: "dev", args: ["big"] }) } catch (e) { m = e.message }
export default m`)
	if code != 0 || !strings.Contains(string(env.Value), "too_expensive:") {
		t.Fatalf("code %d: %s", code, out)
	}
	if cmDoCount("q") != before {
		t.Fatal("expensive read ran")
	}
	// uncaught: the program fails with exit 1
	code, env = codeRun(t, out, `await ops.db.q({ env: "dev", args: ["big"] })`)
	if code != 1 || env.OK || !strings.Contains(env.Error, "too_expensive") {
		t.Fatalf("uncaught: %d %s", code, out)
	}
}

func TestCodeRunBudgets(t *testing.T) {
	out := envSetup(t, codeCfg)
	code, env := codeRun(t, out, `let i = 0; while (true) { i++ }`, "--max-steps", "1000")
	if code != 1 || env.OK {
		t.Fatalf("steps: %d %s", code, out)
	}
	start := time.Now()
	code, env = codeRun(t, out, `await ops.db.q({ env: "dev", args: ["slow"] })`, "--max-time", "300ms")
	if code != 1 || env.OK || time.Since(start) > 4*time.Second {
		t.Fatalf("time: %d %s", code, out)
	}
	// The abandoned call may still be running in-process; wait for it so it
	// does not write the audit log into a removed temp dir.
	select {
	case <-cmSlow:
	case <-time.After(10 * time.Second):
		t.Fatal("slow call never returned")
	}
	// Its audit record is written just after Do returns; wait until the
	// tmvp home stops changing.
	waitQuiet(filepath.Join(os.Getenv("HOME"), ".tmvp"))
}

// waitQuiet waits (max 3s) until no file under dir changed for 200ms.
func waitQuiet(dir string) {
	last, stable := "", 0
	for i := 0; i < 60 && stable < 4; i++ {
		var b strings.Builder
		_ = filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
			if err == nil {
				fmt.Fprintf(&b, "%s %d %d\n", p, fi.Size(), fi.ModTime().UnixNano())
			}
			return nil
		})
		if b.String() == last {
			stable++
		} else {
			last, stable = b.String(), 0
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestCodeCheckExit2(t *testing.T) {
	out := envSetup(t, codeCfg)
	p := filepath.Join(t.TempDir(), "bad.js")
	_ = os.WriteFile(p, []byte(`await ops.db.q({ env: "dev", bogus: 1 }); await ops.db.nope({})`), 0o600)
	if code := run(t, out, "code", "check", p); code != 2 {
		t.Fatalf("check: %d %s", code, out)
	}
	if code := run(t, out, "code", "run", p, "--json"); code != 2 {
		t.Fatalf("run of a bad program must not run: %d %s", code, out)
	}
	if code := run(t, out, "code", "run", "no-such-script"); code != 2 {
		t.Fatalf("missing script: %d %s", code, out)
	}
}

func TestCodeSavedScripts(t *testing.T) {
	out := envSetup(t, codeCfg)
	dir := filepath.Join(filepath.Dir(globals.Config), "scripts")
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "count.js"), []byte("// count rows on every env\nconst r = await ops.db.q({ env: \"all\", args: [\"c\"] })\nexport default r.rows.length\n"), 0o600)
	if code := run(t, out, "code", "scripts"); code != 0 || !strings.Contains(out.String(), "count") || !strings.Contains(out.String(), "count rows on every env") {
		t.Fatalf("scripts: %d %s", code, out)
	}
	if code := run(t, out, "code", "run", "count", "--json"); code != 0 || !strings.Contains(out.String(), `"value": 3`) {
		t.Fatalf("run saved: %d %s", code, out)
	}
}

func TestCodeOperationsNamesAndSchema(t *testing.T) {
	out := envSetup(t, codeCfg)
	if code := run(t, out, "code", "operations"); code != 0 {
		t.Fatalf("%d %s", code, out)
	}
	s := out.String()
	for _, want := range []string{"db.q", "write.db.mut"} {
		if !strings.Contains(s, want) {
			t.Errorf("operations missing %s: %s", want, s)
		}
	}
	if strings.Contains(s, "exec") || strings.Contains(s, "shell") {
		t.Errorf("exec/shell op exposed: %s", s)
	}
}

func compact(b []byte) string {
	var buf bytes.Buffer
	_ = json.Compact(&buf, b)
	return buf.String()
}

func TestCodeServerRefusesWriteWithoutReason(t *testing.T) {
	envSetup(t, codeCfg)
	cfg, _ := loadConfig()
	before := cmDoCount("mut")
	_, err := codeops.Send(context.Background(), client.Do, protocol.Request{Op: protocol.OpWrite,
		ConfigPath: cfg.Path, Env: "dev", System: "db", Verb: "mut", Args: []string{"x"}})
	if err == nil || !strings.HasPrefix(err.Error(), "policy_blocked: tmvp write requires --reason") {
		t.Fatalf("got %v", err)
	}
	if cmDoCount("mut") != before {
		t.Fatal("write without reason reached the connector")
	}
}
