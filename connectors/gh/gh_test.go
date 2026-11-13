package gh

import (
	"context"
	"strings"
	"testing"

	"github.com/muthuishere/tmvp/connectors/internal/cli/clitest"
	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

// The fake echoes what it was called with; a FAKE_ value on argv is a leak.
const fake = `
case "$*" in *FAKE_*) echo "secret on argv" >&2; exit 9;; esac
case "$*" in
  "api user"*) [ "$NOLOGIN" = 1 ] && { echo "To get started with GitHub CLI, please run:  gh auth login" >&2; exit 4; }
               printf '{"login":"octo","name":"Octo","tok":"%s"}\n' "$GH_TOKEN";;
  "run list"*) printf '[{"databaseId":1,"workflowName":"deploy","conclusion":"success","args":"%s","repo":"%s"}]\n' "$*" "$GH_REPO";;
  "run view 7 --json"*) echo '{"databaseId":7,"conclusion":"failure","jobs":[{"name":"build","conclusion":"failure","steps":[{"name":"test","conclusion":"failure"},{"name":"lint","conclusion":"success"}]}]}';;
  "run view 7 --log-failed"*) printf 'build\ttest\tFAIL x\nbuild\ttest\tFAIL y\nbuild\ttest\tFAIL z\n';;
  "api repos/{owner}/{repo}/deployments"*) echo '[{"id":5,"environment":"production","ref":"main","sha":"abc","creator":{"login":"octo"},"created_at":"2026-01-01T00:00:00Z"}]';;
  *) printf '{"args":"%s"}\n' "$*";;
esac`

func open(t *testing.T, mode connector.Mode, f connector.StaticFields) *GH {
	t.Helper()
	if f == nil {
		f = connector.StaticFields{}
	}
	if _, ok := f["repo"]; !ok {
		f["repo"] = "acme/app"
	}
	g := New().(*GH)
	if err := g.Open(context.Background(), connector.NewTarget(connector.Target{System: "gh", Mode: mode}, f)); err != nil {
		t.Fatal(err)
	}
	return g
}

func TestPolicy(t *testing.T) {
	g := New().(connector.Guard)
	for _, ok := range []string{"run list", "run view 12 --log", "pr view 3", "pr checks 3", "release list", "workflow list",
		"api repos/acme/app/actions/runs", "api -X GET repos/acme/app/commits -f per_page=5", "secret list", "variable list", "repo view"} {
		if err := g.Guard(connector.Call{Verb: "call", Args: strings.Fields(ok)}); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"api repos/acme/app/issues -f title=x", "api -X POST repos/x", "api --method=DELETE repos/x",
		"api repos/x --input body.json", "run rerun 5", "pr merge 3", "workflow run deploy", "secret set X",
		"auth token", "auth status --show-token", "config set editor vim", "run list -R other/repo", "run list --repo=other/repo",
		"run download 5", "repo delete acme/app", "repo view other/repo", "ssh-key list", "extension install x"} {
		if err := g.Guard(connector.Call{Verb: "call", Args: strings.Fields(bad)}); errs.CodeOf(err) != errs.PolicyBlocked {
			t.Errorf("%s: want policy_blocked, got %v", bad, err)
		}
	}
	// through tmvp write: mutations pass, secret/local-state stays blocked
	if err := g.Guard(connector.Call{Verb: "cli", Args: strings.Fields("pr merge 3 --squash")}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"secret set X", "auth login", "secret delete X"} {
		if err := g.Guard(connector.Call{Verb: "cli", Args: strings.Fields(bad)}); errs.CodeOf(err) != errs.PolicyBlocked {
			t.Errorf("cli %s: want policy_blocked, got %v", bad, err)
		}
	}
}

func TestRepoSubcommandTakesPositional(t *testing.T) {
	g := &GH{t: connector.NewTarget(connector.Target{}, connector.StaticFields{"repo": "acme/app"})}
	for in, want := range map[string]string{
		"repo view --json name": "repo view acme/app --json name",
		"run list -L 3":         "run list -L 3 -R acme/app",
	} {
		if got := strings.Join(g.withRepo(strings.Fields(in)), " "); got != want {
			t.Errorf("%s: got %q want %q", in, got, want)
		}
	}
}

func TestReadsAndRepo(t *testing.T) {
	clitest.Fake(t, "gh", fake)
	g := open(t, connector.ModeRead, nil)
	ctx := context.Background()
	var c connector.Collector
	if err := g.Do(ctx, connector.Call{Verb: "runs", Args: []string{"--workflow", "deploy"}, Limit: 3}, &c); err != nil {
		t.Fatal(err)
	}
	args, _ := c.Rows[0]["args"].(string)
	if !strings.Contains(args, "-L 3") || !strings.Contains(args, "--workflow deploy") || !strings.HasSuffix(args, "-R acme/app") || c.Rows[0]["repo"] != "acme/app" {
		t.Fatalf("runs %v", c.Rows)
	}
	c = connector.Collector{}
	if err := g.Do(ctx, connector.Call{Verb: "run", Args: []string{"7"}}, &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Rows) != 2 || c.Rows[1]["kind"] != "job" || c.Rows[1]["failed_steps"] != "test" {
		t.Fatalf("run %v", c.Rows)
	}
	c = connector.Collector{}
	if err := g.Do(ctx, connector.Call{Verb: "run-log", Args: []string{"7", "--failed"}, Limit: 2}, &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Lines) != 3 || !strings.Contains(c.Lines[2], "capped") {
		t.Fatalf("run-log %q", c.Lines)
	}
	c = connector.Collector{}
	if err := g.Do(ctx, connector.Call{Verb: "deployments"}, &c); err != nil || c.Rows[0]["environment"] != "production" || c.Rows[0]["creator"] != "octo" {
		t.Fatalf("deployments %v %v", c.Rows, err)
	}
	c = connector.Collector{}
	if err := g.Do(ctx, connector.Call{Verb: "ls"}, &c); err != nil || c.Rows[0]["repo"] != "acme/app" || c.Rows[len(c.Rows)-1]["login"] != "octo" {
		t.Fatalf("ls %v %v", c.Rows, err)
	}
	if err := g.Do(ctx, connector.Call{Verb: "rerun", Args: []string{"7"}}, &c); errs.CodeOf(err) != errs.PolicyBlocked {
		t.Fatalf("rerun in read mode: %v", err)
	}
}

func TestTokenViaEnvOnly(t *testing.T) {
	clitest.Fake(t, "gh", fake)
	g := open(t, connector.ModeRead, connector.StaticFields{"token": "FAKE_TOKEN_123"})
	var c connector.Collector
	if err := g.Do(context.Background(), connector.Call{Verb: "whoami"}, &c); err != nil {
		t.Fatal(err)
	}
	if c.Rows[0]["login"] != "octo" {
		t.Fatalf("whoami %v", c.Rows)
	}
	// `call api user` passes the fake's raw JSON through: GH_TOKEN reached the env (argv would have exited 9)
	c = connector.Collector{}
	if err := g.Do(context.Background(), connector.Call{Verb: "call", Args: []string{"api", "user"}}, &c); err != nil || c.Rows[0]["tok"] != "FAKE_TOKEN_123" {
		t.Fatalf("token not in env: %v", err)
	}
	// without token: the user's login, and a stray GH_TOKEN in tmvp's env is not used
	t.Setenv("GH_TOKEN", "FAKE_STRAY")
	c = connector.Collector{}
	if err := open(t, connector.ModeRead, nil).Do(context.Background(), connector.Call{Verb: "call", Args: []string{"api", "user"}}, &c); err != nil || c.Rows[0]["tok"] != "" {
		t.Fatalf("stray token used: %v", err)
	}
	t.Setenv("NOLOGIN", "1")
	if err := open(t, connector.ModeRead, nil).Do(context.Background(), connector.Call{Verb: "whoami"}, &c); errs.CodeOf(err) != errs.LoginRequired {
		t.Fatalf("not logged in: %v", err)
	}
}

func TestWrites(t *testing.T) {
	clitest.Fake(t, "gh", fake)
	g := open(t, connector.ModeWrite, nil)
	ctx := context.Background()
	var c connector.Collector
	if err := g.Do(ctx, connector.Call{Verb: "dispatch", Args: []string{"deploy-dev.yml", "--ref", "main", "-f", "x=1"}, Preview: true}, &c); err != nil {
		t.Fatal(err)
	}
	if c.Rows[0]["would_run"] != "gh workflow run deploy-dev.yml --ref main -f x=1 -R acme/app" {
		t.Fatalf("preview %v", c.Rows)
	}
	c = connector.Collector{}
	if err := g.Do(ctx, connector.Call{Verb: "rerun", Args: []string{"7", "--failed"}}, &c); err != nil || c.Rows[0]["args"] != "run rerun 7 --failed -R acme/app" {
		t.Fatalf("rerun %v %v", c.Rows, err)
	}
	if err := g.Do(ctx, connector.Call{Verb: "cli", Args: []string{"secret", "set", "X"}}, &c); errs.CodeOf(err) != errs.PolicyBlocked {
		t.Fatalf("secret set via write: %v", err)
	}
}

func TestBadRepo(t *testing.T) {
	clitest.Fake(t, "gh", fake)
	for _, r := range []string{"", "acme", "a/b/c", "/x"} {
		g := New().(*GH)
		if err := g.Open(context.Background(), connector.NewTarget(connector.Target{System: "gh"}, connector.StaticFields{"repo": r})); errs.CodeOf(err) != errs.Config {
			t.Errorf("repo %q: %v", r, err)
		}
	}
}
