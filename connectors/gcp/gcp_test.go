package gcp

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/muthuishere/tmvp/connectors/internal/cli/clitest"
	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

const fake = `
case "$*" in
  *fail-auth*) echo "ERROR: (gcloud) There was a problem refreshing your current auth tokens: invalid_grant" >&2; exit 1;;
  "logging read"*) echo '[{"timestamp":"2026-01-01T00:00:00Z","severity":"ERROR","logName":"projects/p/logs/run.googleapis.com%2Fstderr","resource":{"type":"cloud_run_revision"},"textPayload":"boom"},{"timestamp":"2026-01-01T00:00:01Z","jsonPayload":{"message":"hi"}}]';;
  "config list"*) echo '{"core":{"account":"me@example.com","project":"'"$CLOUDSDK_CORE_PROJECT"'"}}';;
  *) printf '{"project":"%s","override":"%s","cfg":"%s","sa":"%s","args":"%s"}\n' "$CLOUDSDK_CORE_PROJECT" "${CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE:+set}" "$CLOUDSDK_CONFIG" "$(cat "$GOOGLE_APPLICATION_CREDENTIALS" 2>/dev/null | tr -d '"{}')" "$*";;
esac`

func open(t *testing.T, f connector.StaticFields) *GCP {
	t.Helper()
	g := New().(*GCP)
	if err := g.Open(context.Background(), connector.NewTarget(connector.Target{System: "cloud", Mode: connector.ModeRead}, f)); err != nil {
		t.Fatal(err)
	}
	return g
}

func TestVerbs(t *testing.T) {
	if v, _ := connector.DefaultWriteVerb(New().Verbs()); v.Name != "cli" {
		t.Fatal("default write verb")
	}
	for _, v := range New().Verbs() {
		if (v.Name == "cli") != (v.Mode == connector.Write) {
			t.Errorf("%s: %v", v.Name, v.Mode)
		}
	}
}

func TestPolicy(t *testing.T) {
	g := New().(connector.Guard)
	for _, ok := range []string{"compute instances list", "run services describe api --region x", "logging read severity>=ERROR",
		"projects get-iam-policy p", "container clusters list"} {
		if err := g.Guard(connector.Call{Verb: "call", Args: strings.Fields(ok)}); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"compute instances delete vm1", "run deploy api --image x", "config set project p",
		"secrets versions access latest --secret s", "auth print-access-token", "container clusters get-credentials c",
		"compute instances delete list", "storage cp a b", "compute instances list --account other@x"} {
		if err := g.Guard(connector.Call{Verb: "call", Args: strings.Fields(bad)}); errs.CodeOf(err) != errs.PolicyBlocked {
			t.Errorf("%s: want policy_blocked, got %v", bad, err)
		}
	}
	if err := g.Guard(connector.Call{Verb: "cli", Args: strings.Fields("run deploy api --image x")}); err != nil {
		t.Fatal(err)
	}
	if err := g.Guard(connector.Call{Verb: "cli", Args: strings.Fields("config set project p")}); errs.CodeOf(err) != errs.PolicyBlocked {
		t.Fatal("cli must not change gcloud global state")
	}
}

func TestCredentialsIsolated(t *testing.T) {
	clitest.Fake(t, "gcloud", fake)
	g := open(t, connector.StaticFields{"project": "proj-1", "credentials": `{"type":"service_account","client_email":"sa@p.iam"}`})
	var c connector.Collector
	if err := g.Do(context.Background(), connector.Call{Verb: "call", Args: []string{"compute", "instances", "list"}}, &c); err != nil {
		t.Fatal(err)
	}
	r := c.Rows[0]
	if r["project"] != "proj-1" || r["override"] != "set" || !strings.Contains(r["sa"].(string), "service_account") || !strings.Contains(r["args"].(string), "--format=json") {
		t.Fatalf("row %v", r)
	}
	if _, err := os.Stat(r["cfg"].(string)); !os.IsNotExist(err) {
		t.Fatal("CLOUDSDK_CONFIG not cleaned up")
	}
	c = connector.Collector{}
	if err := g.Do(context.Background(), connector.Call{Verb: "whoami"}, &c); err != nil || c.Rows[0]["account"] != "sa@p.iam" {
		t.Fatalf("whoami %v %v", c.Rows, err)
	}
}

func TestLogs(t *testing.T) {
	clitest.Fake(t, "gcloud", fake)
	g := open(t, connector.StaticFields{"project": "p"})
	var c connector.Collector
	if err := g.Do(context.Background(), connector.Call{Verb: "logs", Args: []string{"severity>=ERROR"}, Since: 2 * time.Hour}, &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Rows) != 2 || c.Rows[0]["message"] != "boom" || c.Rows[1]["message"] != "hi" {
		t.Fatalf("rows %v", c.Rows)
	}
}

func TestAuthError(t *testing.T) {
	clitest.Fake(t, "gcloud", fake)
	g := open(t, connector.StaticFields{"credentials": "FAKE_SECRET_123"})
	err := g.Do(context.Background(), connector.Call{Verb: "call", Args: []string{"projects", "list", "--fail-auth"}}, &connector.Collector{})
	if errs.CodeOf(err) != errs.Auth || strings.Contains(err.Error(), "FAKE_SECRET_123") {
		t.Fatalf("got %v", err)
	}
}

func TestLive(t *testing.T) {
	p := os.Getenv("TMVP_TEST_GCP_PROJECT")
	if p == "" {
		t.Skip("TMVP_TEST_GCP_PROJECT not set")
	}
	g := open(t, connector.StaticFields{"project": p})
	if err := g.Do(context.Background(), connector.Call{Verb: "whoami"}, &connector.Collector{}); err != nil {
		t.Fatal(err)
	}
}

func TestPolicySecretOps(t *testing.T) {
	g := New().(connector.Guard)
	for _, bad := range []string{"sql generate-login-token", "iam service-accounts keys create k.json --iam-account a@x",
		"storage hmac create a@x", "compute reset-windows-password vm1", "auth application-default print-access-token"} {
		for _, verb := range []string{"call", "cli"} {
			if err := g.Guard(connector.Call{Verb: verb, Args: strings.Fields(bad)}); errs.CodeOf(err) != errs.PolicyBlocked {
				t.Errorf("%s %s: want policy_blocked, got %v", verb, bad, err)
			}
		}
	}
	if err := g.Guard(connector.Call{Verb: "call", Args: strings.Fields("iam service-accounts keys list --iam-account a@x")}); err != nil {
		t.Errorf("keys list is a read: %v", err)
	}
}
