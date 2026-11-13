package nats

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"

	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

func TestVerbs(t *testing.T) {
	f, ok := connector.Lookup("nats")
	if !ok {
		t.Fatal("not registered")
	}
	want := map[string]connector.VerbMode{"streams": connector.Read, "consumers": connector.Read, "sub": connector.Read, "kv-get": connector.Read,
		"pub": connector.Write, "kv-put": connector.Write, "purge": connector.Destructive, "stream-delete": connector.Destructive}
	for n, m := range want {
		if v, ok := connector.FindVerb(f().Verbs(), n); !ok || v.Mode != m {
			t.Errorf("verb %s: %+v", n, v)
		}
	}
}

func TestGuard(t *testing.T) {
	n := &NATS{}
	cases := []struct {
		c    connector.Call
		want errs.Code
	}{
		{connector.Call{Verb: "purge", Args: []string{"ORDERS"}}, errs.PolicyBlocked},
		{connector.Call{Verb: "stream-delete", Args: []string{"ORDERS"}, Destructive: true}, ""},
		{connector.Call{Verb: "pub", Args: []string{"orders.*", "x"}}, errs.Usage},
		{connector.Call{Verb: "pub", Args: []string{"orders.new"}}, errs.Usage},
		{connector.Call{Verb: "sub", Args: []string{"orders.>"}, Flags: map[string]string{"wait": "nope"}}, errs.Usage},
		{connector.Call{Verb: "sub", Args: []string{"orders.>"}, Flags: map[string]string{"wait": "2s"}}, ""},
	}
	for _, c := range cases {
		if got := errs.CodeOf(n.Guard(c.c)); got != c.want {
			t.Errorf("%+v → %s want %s", c.c, got, c.want)
		}
	}
	if d, _ := wait(connector.Call{Flags: map[string]string{"wait": "10m"}}, 30*time.Second); d != 30*time.Second {
		t.Fatalf("wait cap %v", d)
	}
}

func TestWriteRefusedInReadMode(t *testing.T) {
	n := &NATS{t: connector.NewTarget(connector.Target{Mode: connector.ModeRead}, connector.StaticFields{})}
	for _, c := range []connector.Call{{Verb: "pub", Args: []string{"a", "b"}}, {Verb: "kv-put", Args: []string{"b", "k", "v"}}, {Verb: "purge", Args: []string{"S"}, Destructive: true}} {
		if err := n.Do(context.Background(), c, &connector.Collector{}); errs.CodeOf(err) != errs.PolicyBlocked {
			t.Errorf("%s: %v", c.Verb, err)
		}
	}
}

func TestAuthOptions(t *testing.T) {
	kp, _ := nkeys.CreateUser()
	seed, _ := kp.Seed()
	creds := fmt.Sprintf("-----BEGIN NATS USER JWT-----\nFAKE.JWT.VALUE\n------END NATS USER JWT------\n\n-----BEGIN USER NKEY SEED-----\n%s\n------END USER NKEY SEED------\n", seed)
	tg := connector.NewTarget(connector.Target{Mode: connector.ModeRead}, connector.StaticFields{"read": map[string]any{"creds": creds}})
	o, err := authOptions(context.Background(), tg)
	if err != nil || len(o) != 1 {
		t.Fatalf("creds: %v", err)
	}
	var opts natsgo.Options
	_ = o[0](&opts)
	if j, _ := opts.UserJWT(); j != "FAKE.JWT.VALUE" {
		t.Fatalf("jwt %q", j)
	}
	tg = connector.NewTarget(connector.Target{Mode: connector.ModeRead}, connector.StaticFields{"read": map[string]any{"creds": "FAKE_SECRET_123"}})
	if _, err = authOptions(context.Background(), tg); errs.CodeOf(err) != errs.Config || strings.Contains(err.Error(), "FAKE_SECRET_123") {
		t.Fatalf("bad creds: %v", err)
	}
	tg = connector.NewTarget(connector.Target{Mode: connector.ModeRead}, connector.StaticFields{"read": map[string]any{"token": "FAKE_SECRET_123"}})
	if o, err = authOptions(context.Background(), tg); err != nil || len(o) != 1 {
		t.Fatal(err)
	}
}

func TestClassify(t *testing.T) {
	cases := map[error]errs.Code{
		natsgo.ErrAuthorization: errs.Auth,
		natsgo.ErrNoServers:     errs.Unreachable,
		natsgo.ErrTimeout:       errs.Timeout,
		fmt.Errorf("nats: permissions violation for publish to \"x\""): errs.PolicyBlocked,
	}
	for e, want := range cases {
		if got := errs.CodeOf(classify(e)); got != want {
			t.Errorf("%v → %s want %s", e, got, want)
		}
	}
}

// TestLive: TMVP_TEST_NATS_URL (JetStream enabled). sec run TMVP_TEST_NATS_URL -- go test ./connectors/nats -run Live
func TestLive(t *testing.T) {
	u := os.Getenv("TMVP_TEST_NATS_URL")
	if u == "" {
		t.Skip("TMVP_TEST_NATS_URL not set")
	}
	ctx := context.Background()
	open := func(mode connector.Mode) *NATS {
		n := &NATS{}
		if err := n.Open(ctx, connector.NewTarget(connector.Target{Mode: mode}, connector.StaticFields{string(mode): map[string]any{"url": u}})); err != nil {
			t.Fatal(err)
		}
		return n
	}
	r, w := open(connector.ModeRead), open(connector.ModeWrite)
	defer r.Close()
	defer w.Close()
	var c connector.Collector
	if err := r.Do(ctx, connector.Call{Verb: "streams"}, &c); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	c = connector.Collector{}
	go func() {
		done <- r.Do(ctx, connector.Call{Verb: "sub", Args: []string{"tmvp.live"}, Limit: 1, Flags: map[string]string{"wait": "3s"}}, &c)
	}()
	time.Sleep(300 * time.Millisecond)
	if err := w.Do(ctx, connector.Call{Verb: "pub", Args: []string{"tmvp.live", "hello"}}, &connector.Collector{}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil || len(c.Rows) != 1 || c.Rows[0]["data"] != "hello" {
		t.Fatalf("sub: %v %+v", err, c.Rows)
	}
}
