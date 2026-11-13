package script

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

func setup(t *testing.T, mode connector.Mode, lim connector.Limits) (*Script, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell scripts")
	}
	dir := t.TempDir()
	sd := filepath.Join(dir, "assets", "jobs", "scripts")
	if err := os.MkdirAll(sd, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"status.sh":  "#!/bin/sh\n# tmvp: read\necho \"env=$TMVP_ENV region=$REGION key=$(cat \"$KEYFILE\") args=$*\"\n",
		"migrate.sh": "#!/bin/sh\necho migrating\n",
		"loud.sh":    "# tmvp: read\ni=0; while [ $i -lt 100 ]; do echo line $i; i=$((i+1)); done\n",
		"fail.sh":    "# tmvp: read\necho 'Error: authentication failed' >&2; exit 3\n",
	}
	for n, b := range files {
		if err := os.WriteFile(filepath.Join(sd, n), []byte(b), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	s := New().(*Script)
	tg := connector.NewTarget(connector.Target{Env: "prod", System: "jobs", Mode: mode, Dir: dir, Limits: lim},
		connector.StaticFields{"env": map[string]any{"REGION": "ap-south-1", "KEYFILE": "${file:secrets.k}"}})
	if err := s.Open(context.Background(), tg); err != nil {
		t.Fatal(err)
	}
	return s, sd
}

func TestVerbs(t *testing.T) {
	if v, _ := connector.DefaultWriteVerb(New().Verbs()); v.Name != "exec" {
		t.Fatal("default write verb")
	}
}

func TestReadScript(t *testing.T) {
	s, _ := setup(t, connector.ModeRead, connector.Limits{})
	var c connector.Collector
	if err := s.Do(context.Background(), connector.Call{Verb: "run", Args: []string{"@status", "a1"}}, &c); err != nil {
		t.Fatal(err)
	}
	// StaticFields.FilePath writes the literal field value; a real config gets the secret bytes.
	if len(c.Lines) != 1 || !strings.Contains(c.Lines[0], "env=prod region=ap-south-1 key=${file:secrets.k} args=a1") {
		t.Fatalf("lines %v", c.Lines)
	}
}

func TestUndeclaredScriptIsWrite(t *testing.T) {
	s, _ := setup(t, connector.ModeRead, connector.Limits{})
	err := s.Do(context.Background(), connector.Call{Verb: "run", Args: []string{"@migrate"}}, &connector.Collector{})
	if errs.CodeOf(err) != errs.PolicyBlocked || !strings.Contains(errs.As(err).Hint, "tmvp write") {
		t.Fatalf("got %v", err)
	}
	w, _ := setup(t, connector.ModeWrite, connector.Limits{})
	var c connector.Collector
	if err := w.Do(context.Background(), connector.Call{Verb: "exec", Args: []string{"@migrate"}, Preview: true}, &c); err != nil || c.Rows[0]["preview"] != true {
		t.Fatalf("preview %v %v", c.Rows, err)
	}
	c = connector.Collector{}
	if err := w.Do(context.Background(), connector.Call{Verb: "exec", Args: []string{"@migrate"}}, &c); err != nil || c.Lines[0] != "migrating" {
		t.Fatalf("exec %v %v", c.Lines, err)
	}
}

func TestListAndBounds(t *testing.T) {
	s, _ := setup(t, connector.ModeRead, connector.Limits{Lines: 10, Bytes: 1 << 20, Timeout: 5e9, Rows: 10})
	var c connector.Collector
	if err := s.Do(context.Background(), connector.Call{Verb: "list"}, &c); err != nil || len(c.Rows) != 4 {
		t.Fatalf("list %v %v", c.Rows, err)
	}
	c = connector.Collector{}
	if err := s.Do(context.Background(), connector.Call{Verb: "run", Args: []string{"@loud"}}, &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Lines) != 11 || !strings.Contains(c.Lines[10], "capped") {
		t.Fatalf("lines %d %v", len(c.Lines), c.Lines)
	}
	if err := s.Do(context.Background(), connector.Call{Verb: "run", Args: []string{"@fail"}}, &c); errs.CodeOf(err) != errs.Runtime {
		t.Fatalf("fail: %v", err)
	}
}

func TestBadNames(t *testing.T) {
	g := New().(connector.Guard)
	for _, n := range []string{"@../x", "@a/b", "@", "@.hidden"} {
		if err := g.Guard(connector.Call{Verb: "run", Args: []string{n}}); errs.CodeOf(err) != errs.Usage {
			t.Errorf("%s: %v", n, err)
		}
	}
}

func TestHeaderStyles(t *testing.T) {
	d := t.TempDir()
	for i, h := range []string{"# tmvp: read", "// tmvp: read", "-- TMVP: Read", "rem tmvp: read", "#!/bin/sh\n#tmvp:read"} {
		p := filepath.Join(d, "s"+string(rune('a'+i)))
		os.WriteFile(p, []byte(h+"\necho\n"), 0o644)
		if !IsRead(p) {
			t.Errorf("%q not detected", h)
		}
	}
	p := filepath.Join(d, "w")
	os.WriteFile(p, []byte("echo '# tmvp: read'\n"), 0o644)
	if IsRead(p) {
		t.Error("header inside code counted")
	}
}

func TestCallTimeoutOverridesLimits(t *testing.T) {
	s, sd := setup(t, connector.ModeRead, connector.Limits{Timeout: time.Hour})
	if err := os.WriteFile(filepath.Join(sd, "slow.sh"), []byte("#!/bin/sh\n# tmvp: read\nexec sleep 5\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err := s.Do(context.Background(), connector.Call{Verb: "run", Args: []string{"@slow"}, Timeout: 200 * time.Millisecond}, &connector.Collector{})
	if err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("want Call.Timeout to cut the script, got %v after %s", err, time.Since(start))
	}
}
