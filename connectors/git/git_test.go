package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@x", "GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@x", "GIT_CONFIG_GLOBAL="+emptyConfigPath())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}

func repo(t *testing.T) (string, *Git) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("TMVP_HOME", t.TempDir())
	src := t.TempDir()
	run(t, src, "init", "-q", "-b", "main")
	for i, msg := range []string{"first", "second"} {
		if err := os.WriteFile(filepath.Join(src, "f.txt"), []byte(strings.Repeat("x\n", i+1)), 0o644); err != nil {
			t.Fatal(err)
		}
		run(t, src, "add", ".")
		run(t, src, "commit", "-q", "-m", msg)
	}
	run(t, src, "tag", "-a", "v1.0.0", "-m", "release 1")
	g := New().(*Git)
	tg := connector.NewTarget(connector.Target{Product: "p", Env: "prod", System: "infra", Mode: connector.ModeRead}, connector.StaticFields{"url": src})
	if err := g.Open(context.Background(), tg); err != nil {
		t.Fatal(err)
	}
	return src, g
}

func TestVerbsAllRead(t *testing.T) {
	for _, v := range New().Verbs() {
		if v.Mode != connector.Read {
			t.Errorf("%s is not read", v.Name)
		}
	}
}

func TestLogTagsDiffDeploys(t *testing.T) {
	src, g := repo(t)
	ctx := context.Background()
	if _, err := os.Stat(filepath.Join(os.Getenv("TMVP_HOME"), "p", "git", "infra", "prod", "HEAD")); err != nil {
		t.Fatal("cache not under TMVP_HOME/<product>/git/<system>/<env>")
	}
	var c connector.Collector
	if err := g.Do(ctx, connector.Call{Verb: "log"}, &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Rows) != 2 || c.Rows[0]["subject"] != "second" {
		t.Fatalf("log %v", c.Rows)
	}
	c = connector.Collector{}
	if err := g.Do(ctx, connector.Call{Verb: "tags"}, &c); err != nil || len(c.Rows) != 1 || c.Rows[0]["tag"] != "v1.0.0" {
		t.Fatalf("tags %v %v", c.Rows, err)
	}
	c = connector.Collector{}
	if err := g.Do(ctx, connector.Call{Verb: "diff"}, &c); err != nil || !strings.Contains(strings.Join(c.Lines, "\n"), "+x") {
		t.Fatalf("diff %v %v", c.Lines, err)
	}
	// new commit upstream shows up after fetch
	run(t, src, "commit", "-q", "--allow-empty", "-m", "third")
	c = connector.Collector{}
	if err := g.Do(ctx, connector.Call{Verb: "fetch"}, &c); err != nil {
		t.Fatal(err)
	}
	c = connector.Collector{}
	if err := g.Do(ctx, connector.Call{Verb: "deploys", Since: time.Hour}, &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Rows) != 4 || c.Rows[0]["kind"] != "tag" || c.Rows[1]["subject"] != "third" {
		t.Fatalf("deploys %v", c.Rows)
	}
	c = connector.Collector{}
	if err := g.Do(ctx, connector.Call{Verb: "log", Limit: 1}, &c); err != nil || len(c.Rows) != 1 {
		t.Fatalf("limit %v %v", c.Rows, err)
	}
}

func TestOptionsRefused(t *testing.T) {
	for _, a := range [][]string{{"--output=/tmp/x"}, {"HEAD", "-p"}} {
		if err := New().(connector.Guard).Guard(connector.Call{Verb: "diff", Args: a}); errs.CodeOf(err) != errs.Usage {
			t.Errorf("%v: %v", a, err)
		}
	}
	if err := New().(connector.Guard).Guard(connector.Call{Verb: "log", Args: []string{"HEAD", "--", "-weird-path"}}); err != nil {
		t.Fatal(err)
	}
}

func TestCloneFailureHidesUserinfo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("TMVP_HOME", t.TempDir())
	tg := connector.NewTarget(connector.Target{Product: "p", Env: "e", System: "s", Mode: connector.ModeRead},
		connector.StaticFields{"url": "https://user:FAKE_SECRET_123@127.0.0.1:1/repo.git", "token": "FAKE_TOKEN_456"})
	err := New().Open(context.Background(), tg)
	if err == nil {
		t.Fatal("expected failure")
	}
	if strings.Contains(err.Error(), "FAKE_SECRET_123") || strings.Contains(err.Error(), "FAKE_TOKEN_456") {
		t.Fatalf("secret in error: %v", err)
	}
}

func TestLive(t *testing.T) {
	u := os.Getenv("TMVP_TEST_GIT_URL")
	if u == "" {
		t.Skip("TMVP_TEST_GIT_URL not set")
	}
	t.Setenv("TMVP_HOME", t.TempDir())
	g := New()
	if err := g.Open(context.Background(), connector.NewTarget(connector.Target{Product: "p", Env: "e", System: "s"}, connector.StaticFields{"url": u})); err != nil {
		t.Fatal(err)
	}
}
