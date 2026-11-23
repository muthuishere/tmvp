package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCodeTimeoutAboveMaxTimeRefused(t *testing.T) {
	out := envSetup(t, codeCfg)
	before := cmDoCount("q")
	code, env := codeRun(t, out, `
let m = "ran"
try { await ops.db.q({ env: "dev", args: ["big"], timeout: "2h" }) } catch (e) { m = e.message }
export default m`, "--max-time", "30s")
	if code != 0 || !strings.Contains(string(env.Value), "policy_blocked: timeout 2h is above the program budget") {
		t.Fatalf("%d %s", code, out)
	}
	if cmDoCount("q") != before {
		t.Fatal("connector ran")
	}
}

func TestCodeMaxOps(t *testing.T) {
	out := envSetup(t, codeCfg)
	code, env := codeRun(t, out, `
let n = 0
let m = ""
try { for (let i = 0; i < 10; i++) { await ops.db.q({ env: "dev", args: ["x"] }); n++ } } catch (e) { m = e.message }
export default n + " " + m`, "--max-ops", "3")
	if code != 1 || env.OK || !strings.Contains(env.Error, "operation call limit exceeded (limit 3)") {
		t.Fatalf("%d %s", code, out)
	}
}

func TestCodeMaxMemoryFlag(t *testing.T) {
	out := envSetup(t, codeCfg)
	if code := run(t, out, "code", "run", "-", "--max-memory", "-1"); code != 2 {
		t.Fatalf("negative --max-memory accepted: %d %s", code, out)
	}
	code, env := codeRun(t, out, `export default 1`, "--max-memory", "512")
	if code != 0 || !env.OK {
		t.Fatalf("%d %s", code, out)
	}
}

func TestCodeRejectsGlobalEnv(t *testing.T) {
	out := envSetup(t, codeCfg)
	if code := run(t, out, "-e", "prod", "code", "operations"); code != 2 || !strings.Contains(out.String(), "-e/--env does not apply") {
		t.Fatalf("%d %s", code, out)
	}
}

// Regression: clicodemode >= v0.6.2 resolves symlinks of the module root and
// imports, so a link out of the scripts directory cannot be imported.
func TestCodeSymlinkImportEscapeRefused(t *testing.T) {
	out := envSetup(t, codeCfg)
	outside := t.TempDir()
	_ = os.WriteFile(filepath.Join(outside, "evil.js"), []byte("export const x = 1\n"), 0o600)
	dir := filepath.Join(filepath.Dir(globals.Config), "scripts")
	_ = os.MkdirAll(dir, 0o755)
	if err := os.Symlink(filepath.Join(outside, "evil.js"), filepath.Join(dir, "lib.js")); err != nil {
		t.Skip("no symlinks:", err)
	}
	_ = os.WriteFile(filepath.Join(dir, "p.js"), []byte("import { x } from \"./lib.js\"\nexport default x\n"), 0o600)
	if code := run(t, out, "code", "run", "p", "--json"); code == 0 || !strings.Contains(out.String(), "outside the module root") {
		t.Fatalf("%d %s", code, out)
	}
}

func TestCodeStringDoublingFailsFast(t *testing.T) {
	out := envSetup(t, codeCfg)
	start := time.Now()
	code, env := codeRun(t, out, `let s = "x"; while (true) { s = s + s }`)
	if code != 1 || env.OK || time.Since(start) > 5*time.Second {
		t.Fatalf("%d %s", code, out)
	}
}
