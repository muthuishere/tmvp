package az

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/muthuishere/tmvp/connectors/internal/cli/clitest"
	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

// The fake records whether a login happened in the private config dir.
const fake = `
case "$1" in
  login) case "$*" in *FAKE_*) echo "secret on argv" >&2; exit 9;; esac
         for a in "$@"; do case "$a" in --password=@*) pw=$(cat "${a#--password=@}");; esac; done
         case "$pw" in *FAKE_BAD*) echo "AADSTS7000215: Invalid client secret provided." >&2; exit 1;; esac
         [ -n "$pw" ] || { echo "no password file" >&2; exit 8; }
         touch "$AZURE_CONFIG_DIR/logged-in"; exit 0;;
esac
case "$*" in
  "account show"*) printf '{"name":"sub-a","id":"0000","tenantId":"t1","user":{"name":"app","type":"servicePrincipal"},"loggedin":"%s"}\n' "$(ls "$AZURE_CONFIG_DIR" 2>/dev/null)";;
  "monitor activity-log list"*) echo '[{"eventTimestamp":"2026-01-01T00:00:00Z","level":"Informational","operationName":{"value":"x","localizedValue":"Restart VM"},"status":{"value":"Succeeded"},"caller":"me"}]';;
  *) printf '{"args":"%s","dir":"%s"}\n' "$*" "$AZURE_CONFIG_DIR";;
esac`

func open(t *testing.T, f connector.StaticFields) *AZ {
	t.Helper()
	a := New().(*AZ)
	if err := a.Open(context.Background(), connector.NewTarget(connector.Target{System: "cloud", Mode: connector.ModeRead}, f)); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestPolicy(t *testing.T) {
	g := New().(connector.Guard)
	for _, ok := range []string{"vm list", "webapp show -n x -g y", "monitor log-analytics query -w w --analytics-query x", "storage account show-usage"} {
		if err := g.Guard(connector.Call{Verb: "call", Args: strings.Fields(ok)}); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"vm delete -n x", "vm restart -n x", "storage account keys list -n x", "keyvault secret show --name s",
		"account get-access-token", "aks get-credentials -n c", "webapp deployment list-publishing-profiles", "vm list --subscription other"} {
		if err := g.Guard(connector.Call{Verb: "call", Args: strings.Fields(bad)}); errs.CodeOf(err) != errs.PolicyBlocked {
			t.Errorf("%s: want policy_blocked, got %v", bad, err)
		}
	}
	if err := g.Guard(connector.Call{Verb: "cli", Args: strings.Fields("vm restart -n x")}); err != nil {
		t.Fatal(err)
	}
	if err := g.Guard(connector.Call{Verb: "cli", Args: strings.Fields("account set -s x")}); errs.CodeOf(err) != errs.PolicyBlocked {
		t.Fatal("cli must not change az global state")
	}
}

func TestServicePrincipalIsolated(t *testing.T) {
	clitest.Fake(t, "az", fake)
	a := open(t, connector.StaticFields{"subscription": "sub-a", "tenant": "t1", "client_id": "app", "client_secret": "FAKE_SECRET_123"})
	var c connector.Collector
	if err := a.Do(context.Background(), connector.Call{Verb: "whoami"}, &c); err != nil {
		t.Fatal(err)
	}
	if c.Rows[0]["subscription"] != "sub-a" {
		t.Fatalf("row %v", c.Rows)
	}
	c = connector.Collector{}
	if err := a.Do(context.Background(), connector.Call{Verb: "call", Args: []string{"vm", "list"}}, &c); err != nil {
		t.Fatal(err)
	}
	args, dir := c.Rows[0]["args"].(string), c.Rows[0]["dir"].(string)
	if !strings.Contains(args, "--subscription sub-a") || dir == "" {
		t.Fatalf("row %v", c.Rows[0])
	}
	home, _ := os.UserHomeDir()
	if strings.HasPrefix(dir, filepath.Join(home, ".azure")) {
		t.Fatal("used the user's az config")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("AZURE_CONFIG_DIR not cleaned up")
	}
}

func TestBadSecretIsAuthAndNotEchoed(t *testing.T) {
	clitest.Fake(t, "az", fake)
	a := open(t, connector.StaticFields{"tenant": "t1", "client_id": "app", "client_secret": "FAKE_BAD_SECRET"})
	err := a.Do(context.Background(), connector.Call{Verb: "whoami"}, &connector.Collector{})
	if errs.CodeOf(err) != errs.Auth || strings.Contains(err.Error(), "FAKE_BAD_SECRET") {
		t.Fatalf("got %v", err)
	}
}

func TestActivityLog(t *testing.T) {
	clitest.Fake(t, "az", fake)
	a := open(t, connector.StaticFields{})
	var c connector.Collector
	if err := a.Do(context.Background(), connector.Call{Verb: "logs"}, &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Rows) != 1 || c.Rows[0]["operation"] != "Restart VM" || c.Rows[0]["status"] != "Succeeded" {
		t.Fatalf("rows %v", c.Rows)
	}
	if err := a.Do(context.Background(), connector.Call{Verb: "logs", Args: []string{"AppTraces"}}, &c); errs.CodeOf(err) != errs.Config {
		t.Fatalf("kql without workspace: %v", err)
	}
}

func TestLive(t *testing.T) {
	if os.Getenv("TMVP_TEST_AZ_SUBSCRIPTION") == "" {
		t.Skip("TMVP_TEST_AZ_SUBSCRIPTION not set")
	}
	a := open(t, connector.StaticFields{"subscription": os.Getenv("TMVP_TEST_AZ_SUBSCRIPTION")})
	if err := a.Do(context.Background(), connector.Call{Verb: "whoami"}, &connector.Collector{}); err != nil {
		t.Fatal(err)
	}
}
