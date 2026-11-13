package kafka

import (
	"context"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/twmb/franz-go/pkg/kfake"

	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

const fakePW = "FAKE_SECRET_123"

// countingDialer proves every connection goes through Target.Dialer.
type countingDialer struct{ n atomic.Int32 }

func (d *countingDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	d.n.Add(1)
	var nd net.Dialer
	return nd.DialContext(ctx, network, addr)
}

func cluster(t *testing.T, opts ...kfake.Opt) string {
	t.Helper()
	c, err := kfake.NewCluster(append([]kfake.Opt{kfake.NumBrokers(1), kfake.SeedTopics(2, "orders")}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return strings.Join(c.ListenAddrs(), ",")
}

func open(t *testing.T, fields connector.StaticFields, mode connector.Mode) (*Kafka, *countingDialer, error) {
	t.Helper()
	d := &countingDialer{}
	k := New().(*Kafka)
	err := k.Open(context.Background(), connector.NewTarget(connector.Target{System: "bus", Mode: mode, Dialer: d}, fields))
	if err == nil {
		t.Cleanup(func() { k.Close() })
	}
	return k, d, err
}

func TestVerbTable(t *testing.T) {
	modes := map[string]connector.VerbMode{"ls": connector.Read, "consume": connector.Read, "produce": connector.Write,
		"create-topic": connector.Write, "delete-topic": connector.Destructive, "delete-records": connector.Destructive, "reset-offsets": connector.Destructive}
	for name, m := range modes {
		v, ok := connector.FindVerb(New().Verbs(), name)
		if !ok || v.Mode != m {
			t.Errorf("%s: %v %v", name, ok, v.Mode)
		}
	}
	if v, ok := connector.DefaultWriteVerb(New().Verbs()); !ok || v.Name != "produce" {
		t.Errorf("default write verb %v", v)
	}
}

func TestGuard(t *testing.T) {
	k := New().(*Kafka)
	cases := []struct {
		c    connector.Call
		code errs.Code
	}{
		{connector.Call{Verb: "produce", Args: []string{"t", "v"}}, errs.PolicyBlocked},                                   // read cannot mutate
		{connector.Call{Verb: "delete-topic", Args: []string{"t"}, Write: true}, errs.PolicyBlocked},                      // destructive off
		{connector.Call{Verb: "consume", Args: []string{"t"}, Flags: map[string]string{"from": "yesterday"}}, errs.Usage}, // bad from
		{connector.Call{Verb: "describe", Args: []string{"ord*"}}, errs.Usage},                                            // wildcard
		{connector.Call{Verb: "reset-offsets", Args: []string{"g", "t"}, Write: true, Destructive: true}, errs.Usage},     // no to=
		{connector.Call{Verb: "delete-records", Args: []string{"t"}, Write: true, Destructive: true}, errs.Usage},         // no before=
		{connector.Call{Verb: "lag"}, errs.Usage},                                                                         // missing group
	}
	for _, tc := range cases {
		if err := k.Guard(tc.c); errs.CodeOf(err) != tc.code {
			t.Errorf("%v: got %v want %s", tc.c, err, tc.code)
		}
	}
	for _, f := range []string{"", "earliest", "latest", "latest-5", "42", "2026-01-01T00:00:00Z"} {
		if _, err := parseFrom(f); err != nil {
			t.Errorf("from=%q: %v", f, err)
		}
	}
}

func TestProduceConsumeDescribe(t *testing.T) {
	addr := cluster(t)
	k, d, err := open(t, connector.StaticFields{"brokers": addr}, connector.ModeWrite)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, v := range []string{"a", "b", "c"} {
		var c connector.Collector
		if err := k.Do(ctx, connector.Call{Verb: "produce", Args: []string{"orders", v}, Flags: map[string]string{"key": "k1"}, Write: true}, &c); err != nil {
			t.Fatal(err)
		}
	}
	var c connector.Collector
	if err := k.Do(ctx, connector.Call{Verb: "ls"}, &c); err != nil || len(c.Rows) != 1 || c.Rows[0]["name"] != "orders" || c.Rows[0]["partitions"] != 2 {
		t.Fatalf("ls %v %v", c.Rows, err)
	}
	c = connector.Collector{}
	if err := k.Do(ctx, connector.Call{Verb: "consume", Args: []string{"orders"}, Flags: map[string]string{"from": "earliest"}}, &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Rows) != 3 || c.Rows[0]["key"] != "k1" {
		t.Fatalf("consume %v", c.Rows)
	}
	c = connector.Collector{}
	if err := k.Do(ctx, connector.Call{Verb: "consume", Args: []string{"orders"}, Flags: map[string]string{"from": "latest-1"}}, &c); err != nil || len(c.Rows) != 1 || c.Rows[0]["value"] != "c" {
		t.Fatalf("latest-1 %v %v", c.Rows, err)
	}
	c = connector.Collector{}
	if err := k.Do(ctx, connector.Call{Verb: "describe", Args: []string{"orders"}}, &c); err != nil {
		t.Fatal(err)
	}
	total := int64(0)
	for _, r := range c.Rows {
		if r["kind"] == "partition" {
			total += r["messages"].(int64)
		}
	}
	if total != 3 {
		t.Fatalf("describe %v", c.Rows)
	}
	c = connector.Collector{}
	if err := k.Do(ctx, connector.Call{Verb: "health"}, &c); err != nil || len(c.Rows) != 1 {
		t.Fatalf("health %v %v", c.Rows, err)
	}
	c = connector.Collector{}
	if err := k.Do(ctx, connector.Call{Verb: "create-topic", Args: []string{"audit"}, Flags: map[string]string{"partitions": "3"}, Write: true}, &c); err != nil {
		t.Fatal(err)
	}
	c = connector.Collector{}
	if err := k.Do(ctx, connector.Call{Verb: "delete-topic", Args: []string{"audit"}, Write: true, Destructive: true}, &c); err != nil {
		t.Fatal(err)
	}
	if d.n.Load() == 0 {
		t.Fatal("no connection went through Target.Dialer")
	}
}

func TestSASLFailureIsAuthError(t *testing.T) {
	addr := cluster(t, kfake.EnableSASL(), kfake.Superuser("PLAIN", "admin", fakePW))
	if _, _, err := open(t, connector.StaticFields{"brokers": addr,
		"sasl": map[string]any{"mechanism": "plain", "user": "admin", "password": fakePW}}, connector.ModeRead); err != nil {
		t.Fatalf("good creds: %v", err)
	}
	_, _, err := open(t, connector.StaticFields{"brokers": addr,
		"sasl": map[string]any{"mechanism": "plain", "user": "admin", "password": "FAKE_WRONG_456"}}, connector.ModeRead)
	if errs.CodeOf(err) != errs.Auth {
		t.Fatalf("want auth_error, got %v", err)
	}
	if strings.Contains(err.Error(), "FAKE_WRONG_456") {
		t.Fatal("secret in error")
	}
}

func TestConfigErrors(t *testing.T) {
	for _, f := range []connector.StaticFields{
		{},
		{"brokers": "no-port"},
		{"brokers": "h:9092", "sasl": map[string]any{"mechanism": "gssapi", "user": "u", "password": fakePW}},
		{"brokers": "h:9092", "tls": map[string]any{"ca": "not pem"}},
	} {
		_, _, err := open(t, f, connector.ModeRead)
		if errs.CodeOf(err) != errs.Config {
			t.Errorf("%v: %v", f, err)
		} else if strings.Contains(err.Error(), fakePW) {
			t.Error("secret in error")
		}
	}
}

func TestLive(t *testing.T) {
	brokers := os.Getenv("TMVP_TEST_KAFKA_BROKERS")
	if brokers == "" {
		t.Skip("TMVP_TEST_KAFKA_BROKERS not set")
	}
	f := connector.StaticFields{"brokers": brokers}
	if u := os.Getenv("TMVP_TEST_KAFKA_USER"); u != "" {
		f["sasl"] = map[string]any{"mechanism": os.Getenv("TMVP_TEST_KAFKA_MECHANISM"), "user": u, "password": os.Getenv("TMVP_TEST_KAFKA_PASSWORD")}
	}
	k, _, err := open(t, f, connector.ModeRead)
	if err != nil {
		t.Fatal(err)
	}
	var c connector.Collector
	if err := k.Do(context.Background(), connector.Call{Verb: "health"}, &c); err != nil {
		t.Fatal(err)
	}
	t.Log(c.Rows)
}
