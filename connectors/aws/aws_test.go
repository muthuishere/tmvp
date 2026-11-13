package aws

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

const fakeAWS = `
case "$*" in
  *fail-auth*) echo "An error occurred (ExpiredToken): The security token included in the request is expired" >&2; exit 255;;
  *filter-log-events*) printf '%s\n' '{"events":[{"logStreamName":"s1","timestamp":1700000000000,"message":"hello\n"},{"logStreamName":"s2","timestamp":1700000001000,"message":"world"}]}';;
  *) printf '{"Account":"123456789012","Arn":"arn:aws:iam::123:user/x","key_set":"%s","cfg":"%s","creds":"%s","region":"%s","args":"%s"}\n' \
       "${AWS_ACCESS_KEY_ID:+yes}" "$AWS_CONFIG_FILE" "$(cat "$AWS_SHARED_CREDENTIALS_FILE" 2>/dev/null)" "$AWS_REGION" "$*";;
esac`

func open(t *testing.T, mode connector.Mode, f connector.StaticFields) *AWS {
	t.Helper()
	a := New().(*AWS)
	if err := a.Open(context.Background(), connector.NewTarget(connector.Target{System: "cloud", Mode: mode}, f)); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestVerbs(t *testing.T) {
	want := map[string]connector.VerbMode{"whoami": connector.Read, "logs": connector.Read, "call": connector.ReadWrite, "cli": connector.Write}
	for _, v := range New().Verbs() {
		if want[v.Name] != v.Mode {
			t.Errorf("%s mode %v", v.Name, v.Mode)
		}
		delete(want, v.Name)
	}
	if len(want) != 0 {
		t.Fatalf("missing %v", want)
	}
	if v, _ := connector.DefaultWriteVerb(New().Verbs()); v.Name != "cli" {
		t.Fatal("default write verb")
	}
}

func TestReadPolicy(t *testing.T) {
	g := New().(connector.Guard)
	for _, ok := range [][]string{{"ec2", "describe-instances"}, {"s3", "ls", "s3://b"}, {"logs", "tail", "g"}, {"ssm", "get-parameter", "--name", "x"}, {"dynamodb", "scan", "--table-name", "t"}} {
		if err := g.Guard(connector.Call{Verb: "call", Args: ok}); err != nil {
			t.Errorf("%v: %v", ok, err)
		}
	}
	for _, bad := range [][]string{{"ec2", "terminate-instances"}, {"s3", "rm", "s3://b/x"}, {"s3", "cp", "a", "b"},
		{"secretsmanager", "get-secret-value", "--secret-id", "x"}, {"ssm", "get-parameter", "--name", "x", "--with-decryption"},
		{"ecr", "get-login-password"}, {"s3api", "get-object", "--bucket", "b", "--key", "k", "out"}, {"ec2", "describe-instances", "--profile", "other"}} {
		err := g.Guard(connector.Call{Verb: "call", Args: bad})
		if errs.CodeOf(err) != errs.PolicyBlocked {
			t.Errorf("%v: want policy_blocked, got %v", bad, err)
		}
	}
	err := g.Guard(connector.Call{Verb: "call", Args: []string{"ec2", "terminate-instances"}})
	if !strings.Contains(errs.As(err).Hint, "tmvp write") {
		t.Fatalf("hint should name tmvp write: %v", err)
	}
	if err := g.Guard(connector.Call{Verb: "cli", Args: []string{"ec2", "terminate-instances", "--instance-ids", "i-1"}}); err != nil {
		t.Fatalf("write path: %v", err)
	}
	// `tmvp write prod cloud call ec2 start-instances`: call is ReadWrite, write mode allows mutation
	if err := g.Guard(connector.Call{Verb: "call", Write: true, Args: []string{"ec2", "start-instances", "--instance-ids", "i-1"}}); err != nil {
		t.Fatalf("call through write: %v", err)
	}
}

func TestWhoamiIsolatedKeys(t *testing.T) {
	clitest.Fake(t, "aws", fakeAWS)
	a := open(t, connector.ModeRead, connector.StaticFields{"region": "ap-south-1",
		"read": map[string]any{"access_key": "FAKE_AKID_123", "secret_key": "FAKE_SECRET_123"}})
	var c connector.Collector
	if err := a.Do(context.Background(), connector.Call{Verb: "whoami"}, &c); err != nil {
		t.Fatal(err)
	}
	r := c.Rows[0]
	if r["key_set"] != "yes" || r["region"] != "ap-south-1" || r["creds"] != "" {
		t.Fatalf("row %v", r)
	}
	cfg := r["cfg"].(string)
	home, _ := os.UserHomeDir()
	if cfg == "" || strings.HasPrefix(cfg, home+"/.aws") {
		t.Fatalf("config not isolated: %q", cfg)
	}
	if _, err := os.Stat(cfg); !os.IsNotExist(err) {
		t.Fatal("per-call config dir not cleaned up")
	}
}

func TestCredentialsFile(t *testing.T) {
	clitest.Fake(t, "aws", fakeAWS)
	a := open(t, connector.ModeRead, connector.StaticFields{"credentials": "[default] aws_access_key_id=FAKE"})
	var c connector.Collector
	if err := a.Do(context.Background(), connector.Call{Verb: "call", Args: []string{"ec2", "describe-regions"}}, &c); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(c.Rows[0]["creds"].(string), "aws_access_key_id") || !strings.Contains(c.Rows[0]["args"].(string), "--output json") {
		t.Fatalf("row %v", c.Rows[0])
	}
}

func TestWriteModeNeverUsesReadKeys(t *testing.T) {
	clitest.Fake(t, "aws", fakeAWS)
	a := open(t, connector.ModeWrite, connector.StaticFields{"read": map[string]any{"access_key": "FAKE_RO", "secret_key": "FAKE_RO_S"}})
	var c connector.Collector
	if err := a.Do(context.Background(), connector.Call{Verb: "cli", Args: []string{"ec2", "start-instances"}}, &c); err != nil {
		t.Fatal(err)
	}
	if c.Rows[0]["key_set"] == "yes" {
		t.Fatal("write mode picked up read.access_key")
	}
	c = connector.Collector{}
	if err := a.Do(context.Background(), connector.Call{Verb: "call", Write: true, Preview: true, Args: []string{"ec2", "start-instances"}}, &c); err != nil || c.Rows[0]["preview"] != true {
		t.Fatalf("call through write must honour --preview: %v %v", err, c.Rows)
	}
	c = connector.Collector{}
	if err := a.Do(context.Background(), connector.Call{Verb: "cli", Preview: true, Args: []string{"ec2", "start-instances"}}, &c); err != nil || c.Rows[0]["preview"] != true {
		t.Fatalf("preview %v %v", c.Rows, err)
	}
}

func TestLogs(t *testing.T) {
	clitest.Fake(t, "aws", fakeAWS)
	a := open(t, connector.ModeRead, connector.StaticFields{})
	var c connector.Collector
	if err := a.Do(context.Background(), connector.Call{Verb: "logs", Args: []string{"/app"}, Since: time.Hour}, &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Rows) != 2 || c.Rows[0]["message"] != "hello" {
		t.Fatalf("rows %v", c.Rows)
	}
	if err := a.Do(context.Background(), connector.Call{Verb: "logs", Args: []string{"/app"}, Since: 30 * 24 * time.Hour}, &c); errs.CodeOf(err) != errs.GuardRejected {
		t.Fatalf("since cap: %v", err)
	}
}

func TestAuthErrorNoSecret(t *testing.T) {
	clitest.Fake(t, "aws", fakeAWS)
	a := open(t, connector.ModeRead, connector.StaticFields{"access_key": "FAKE_AKID_123", "secret_key": "FAKE_SECRET_123"})
	err := a.Do(context.Background(), connector.Call{Verb: "call", Args: []string{"sts", "get-caller-identity", "--fail-auth"}}, &connector.Collector{})
	if errs.CodeOf(err) != errs.Auth {
		t.Fatalf("want auth_error, got %v", err)
	}
	if strings.Contains(err.Error(), "FAKE_SECRET_123") || strings.Contains(err.Error(), "FAKE_AKID_123") {
		t.Fatal("secret in error")
	}
}

func TestMissingCLI(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	err := New().Open(context.Background(), connector.NewTarget(connector.Target{}, connector.StaticFields{}))
	if errs.CodeOf(err) != errs.Unsupported {
		t.Fatalf("got %v", err)
	}
}

func TestLive(t *testing.T) {
	if os.Getenv("TMVP_TEST_AWS_PROFILE") == "" {
		t.Skip("TMVP_TEST_AWS_PROFILE not set")
	}
	a := open(t, connector.ModeRead, connector.StaticFields{"profile": os.Getenv("TMVP_TEST_AWS_PROFILE")})
	var c connector.Collector
	if err := a.Do(context.Background(), connector.Call{Verb: "whoami"}, &c); err != nil {
		t.Fatal(err)
	}
}

func TestReadPolicySecretOps(t *testing.T) {
	g := New().(connector.Guard)
	for _, bad := range [][]string{{"eks", "get-token", "--cluster-name", "c"}, {"ec2", "get-password-data", "--instance-id", "i-1"},
		{"redshift", "get-cluster-credentials", "--cluster-identifier", "c"}, {"redshift", "get-cluster-credentials-with-iam"},
		{"lightsail", "get-instance-access-details", "--instance-name", "x"}, {"lambda", "get-function", "--function-name", "f"},
		{"cognito-identity", "get-id", "--identity-pool-id", "p"}, {"cognito-identity", "get-credentials-for-identity"},
		{"cognito-identity", "get-open-id-token"}, {"apigateway", "test-invoke-method"}, {"iam", "test-something"}} {
		if err := g.Guard(connector.Call{Verb: "call", Args: bad}); errs.CodeOf(err) != errs.PolicyBlocked {
			t.Errorf("%v: want policy_blocked, got %v", bad, err)
		}
	}
	if err := g.Guard(connector.Call{Verb: "call", Args: []string{"lambda", "get-function-configuration", "--function-name", "f"}}); err != nil {
		t.Errorf("get-function-configuration should stay a read: %v", err)
	}
}

func TestWritePathBlocksSecretOps(t *testing.T) {
	a := &AWS{}
	for _, args := range [][]string{{"secretsmanager", "get-secret-value", "--secret-id", "x"}, {"ssm", "get-parameter", "--name", "x", "--with-decryption"}} {
		if err := a.Guard(connector.Call{Verb: "cli", Args: args, Write: true}); err == nil {
			t.Fatalf("expected block for %v", args)
		}
	}
}
