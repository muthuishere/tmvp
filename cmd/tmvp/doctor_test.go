package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// doctorEnv isolates HOME/cwd/state and captures stdout.
func doctorEnv(t *testing.T) *bytes.Buffer {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMVP_HOME", t.TempDir())
	if runtime.GOOS != "windows" {
		t.Setenv("TMVP_SOCKET", "/tmp/tmvp-doctor-test-none.sock") // short; nothing listens
	}
	t.Setenv("TMVP_PRODUCT", "")
	if runtime.GOOS != "windows" {
		// only echo on PATH: no real (slow) CLIs get probed
		bin := t.TempDir()
		if e, err := exec.LookPath("echo"); err == nil {
			_ = os.Symlink(e, filepath.Join(bin, "echo"))
		}
		t.Setenv("PATH", bin)
	}
	t.Chdir(t.TempDir())
	var out bytes.Buffer
	stdout, stderr = &out, &out
	old := globals
	t.Cleanup(func() { stdout, stderr, globals = os.Stdout, os.Stderr, old })
	return &out
}

func doctorExit(err error) int {
	var ee exitError
	if errors.As(err, &ee) {
		return ee.code
	}
	if err != nil {
		return -1
	}
	return 0
}

func TestDoctorNoConfigFakeTools(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake tools need exec bits")
	}
	old := toolProbeTimeout
	toolProbeTimeout = 60 * time.Second
	t.Cleanup(func() { toolProbeTimeout = old })
	out := doctorEnv(t)
	bin := t.TempDir()
	fake := "#!/bin/sh\necho 'Vault v0.0.0-FAKE'\necho second line\n"
	if err := os.WriteFile(filepath.Join(bin, "vault"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	err := runDoctor(context.Background(), false, "", "")
	if c := doctorExit(err); c != 0 {
		t.Fatalf("exit %d, want 0\n%s", c, out)
	}
	s := out.String()
	for _, w := range []string{"MACHINE", "os/arch", "socket", "server", "TOOLS", "Vault v0.0.0-FAKE",
		"playwright-cli", "built-in, no tool needed", "postgres", "TYPES", "no config found", "summary: all good"} {
		if !strings.Contains(s, w) {
			t.Errorf("missing %q in\n%s", w, s)
		}
	}
	if strings.Contains(s, "second line") {
		t.Error("version should be the first line only")
	}

	// JSON: every line is a row frame with a section
	out.Reset()
	globals.JSON = true
	_ = runDoctor(context.Background(), true, "", "")
	secs := map[string]bool{}
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var f struct {
			Type string         `json:"type"`
			Row  map[string]any `json:"row"`
		}
		if err := json.Unmarshal([]byte(l), &f); err != nil || f.Type != "row" {
			t.Fatalf("bad JSONL line %q", l)
		}
		secs[f.Row["section"].(string)] = true
	}
	for _, w := range []string{"machine", "tools", "types", "config", "summary"} {
		if !secs[w] {
			t.Errorf("JSON missing section %s", w)
		}
	}
}

func TestDoctorBadConfigExit2(t *testing.T) {
	out := doctorEnv(t)
	globals.Config = filepath.Join(t.TempDir(), "nope.yaml")
	if c := doctorExit(runDoctor(context.Background(), true, "", "")); c != 2 {
		t.Fatalf("exit %d, want 2\n%s", c, out)
	}
}

func TestDoctorSecretOkConnectFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses echo from PATH")
	}
	out := doctorEnv(t)
	t.Setenv("TMVP_NO_SERVER", "1")
	dir := t.TempDir()
	cfg := `product: drtest
secrets:
  pw: "$(echo FAKE_SECRET_123)"
envs:
  dev:
    systems:
      db:
        type: postgres
        read:
          dsn: "postgres://u:${secrets.pw}@127.0.0.1:1/x?sslmode=disable&connect_timeout=3"
`
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	globals.Config = p
	globals.Timeout = "5s"
	for _, js := range []bool{false, true} {
		out.Reset()
		globals.JSON = js
		c := doctorExit(runDoctor(context.Background(), false, "", ""))
		s := out.String()
		if c != 1 {
			t.Fatalf("json=%v exit %d, want 1\n%s", js, c, s)
		}
		if strings.Contains(s, "FAKE_SECRET_123") {
			t.Fatalf("secret value leaked:\n%s", s)
		}
		want := []string{"pw=ok", "failed (unreachable"}
		if js {
			want = []string{`"name":"pw","state":"ok"`, `"connect":"failed"`, `unreachable`}
		}
		for _, w := range want {
			if !strings.Contains(s, w) {
				t.Errorf("json=%v missing %q in\n%s", js, w, s)
			}
		}
	}
	// offline: no secret command, no connection, nothing failing
	out.Reset()
	globals.JSON = false
	if c := doctorExit(runDoctor(context.Background(), true, "dev", "db")); c != 0 {
		t.Fatalf("offline exit %d\n%s", c, out)
	}
	if !strings.Contains(out.String(), "pw=skipped") {
		t.Errorf("offline should skip secrets:\n%s", out)
	}
}

func TestFirstWord(t *testing.T) {
	for in, want := range map[string]string{"sec get X": "sec", "/usr/local/bin/op read op://a": "op", "FOO=1 vault kv get x": "vault", "": ""} {
		if got := firstWord(in); got != want {
			t.Errorf("%q: %q want %q", in, got, want)
		}
	}
}
