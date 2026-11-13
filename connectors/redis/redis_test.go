package redis

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

func TestVerbs(t *testing.T) {
	f, ok := connector.Lookup("redis")
	if !ok {
		t.Fatal("not registered")
	}
	want := map[string]connector.VerbMode{"info": connector.Read, "get": connector.Read, "scan": connector.Read, "ttl": connector.Read, "type": connector.Read,
		"set": connector.Write, "del": connector.Write, "expire": connector.Write, "flushdb": connector.Destructive, "flushall": connector.Destructive}
	for n, m := range want {
		v, ok := connector.FindVerb(f().Verbs(), n)
		if !ok || v.Mode != m {
			t.Errorf("verb %s: %+v", n, v)
		}
	}
}

func TestGuard(t *testing.T) {
	r := &Redis{}
	cases := []struct {
		c    connector.Call
		want errs.Code
	}{
		{connector.Call{Verb: "flushall"}, errs.PolicyBlocked},
		{connector.Call{Verb: "flushdb", Destructive: true}, ""},
		{connector.Call{Verb: "get"}, errs.Usage},
		{connector.Call{Verb: "set", Args: []string{"k", "v", "soon"}}, errs.Usage},
		{connector.Call{Verb: "set", Args: []string{"k", "v", "10m"}}, ""},
		{connector.Call{Verb: "expire", Args: []string{"k", "60"}}, ""},
	}
	for _, c := range cases {
		if got := errs.CodeOf(r.Guard(c.c)); got != c.want {
			t.Errorf("%+v → %s want %s", c.c, got, c.want)
		}
	}
}

func TestWriteRefusedInReadMode(t *testing.T) {
	r := &Redis{t: connector.NewTarget(connector.Target{Mode: connector.ModeRead}, connector.StaticFields{})}
	for _, c := range []connector.Call{{Verb: "set", Args: []string{"k", "v"}}, {Verb: "del", Args: []string{"k"}}, {Verb: "flushdb", Destructive: true}} {
		if err := r.Do(context.Background(), c, &connector.Collector{}); errs.CodeOf(err) != errs.PolicyBlocked {
			t.Errorf("%s: %v", c.Verb, err)
		}
	}
}

func TestOptions(t *testing.T) {
	tg := connector.NewTarget(connector.Target{Mode: connector.ModeRead}, connector.StaticFields{
		"read": map[string]any{"url": "rediss://cache.example:6380/2", "password": "FAKE_SECRET_123"}})
	o, err := options(context.Background(), tg)
	if err != nil || o.Addr != "cache.example:6380" || o.DB != 2 || o.TLSConfig == nil || o.TLSConfig.ServerName != "cache.example" || o.Password != "FAKE_SECRET_123" || o.Dialer == nil {
		t.Fatalf("%+v %v", o, err)
	}
	tg = connector.NewTarget(connector.Target{Mode: connector.ModeWrite}, connector.StaticFields{
		"tls": "true", "write": map[string]any{"url": "redis://h:6379"}})
	if o, err = options(context.Background(), tg); err != nil || o.TLSConfig == nil {
		t.Fatalf("tls flag: %v", err)
	}
	tg = connector.NewTarget(connector.Target{Mode: connector.ModeRead}, connector.StaticFields{"read": map[string]any{"url": "http://u:FAKE_SECRET_123@h"}})
	if _, err = options(context.Background(), tg); err == nil || strings.Contains(err.Error(), "FAKE_SECRET_123") {
		t.Fatalf("%v", err)
	}
}

func TestParseInfoAndTTL(t *testing.T) {
	rows := parseInfo("# Server\r\nredis_version:7.2.4\r\n\r\n# Keyspace\r\ndb0:keys=3,expires=0\r\n")
	if len(rows) != 2 || rows[0]["section"] != "server" || rows[0]["value"] != "7.2.4" {
		t.Fatalf("%+v", rows)
	}
	if r := ttlRow("k", -2*time.Nanosecond); r["exists"] != false {
		t.Fatal(r)
	}
	if r := ttlRow("k", 90*time.Second); r["ttl_s"] != int64(90) {
		t.Fatal(r)
	}
}

func TestClassify(t *testing.T) {
	cases := map[string]errs.Code{
		"WRONGPASS invalid username-password pair or user is disabled.": errs.Auth,
		"NOAUTH Authentication required.":                               errs.Auth,
		"NOPERM this user has no permissions to run the 'set' command":  errs.PolicyBlocked,
		"READONLY You can't write against a read only replica.":         errs.PolicyBlocked,
		"ERR something": errs.Runtime,
	}
	for m, want := range cases {
		if got := errs.CodeOf(classify(errors.New(m))); got != want {
			t.Errorf("%q → %s want %s", m, got, want)
		}
	}
}

// TestLive: TMVP_TEST_REDIS_URL. sec run TMVP_TEST_REDIS_URL -- go test ./connectors/redis -run Live
func TestLive(t *testing.T) {
	u := os.Getenv("TMVP_TEST_REDIS_URL")
	if u == "" {
		t.Skip("TMVP_TEST_REDIS_URL not set")
	}
	ctx := context.Background()
	open := func(mode connector.Mode) *Redis {
		r := &Redis{}
		if err := r.Open(ctx, connector.NewTarget(connector.Target{Mode: mode}, connector.StaticFields{string(mode): map[string]any{"url": u}})); err != nil {
			t.Fatal(err)
		}
		return r
	}
	w := open(connector.ModeWrite)
	defer w.Close()
	var c connector.Collector
	if err := w.Do(ctx, connector.Call{Verb: "set", Args: []string{"tmvp:live", "v", "60"}}, &c); err != nil {
		t.Fatal(err)
	}
	r := open(connector.ModeRead)
	defer r.Close()
	c = connector.Collector{}
	if err := r.Do(ctx, connector.Call{Verb: "get", Args: []string{"tmvp:live"}}, &c); err != nil || c.Rows[0]["value"] != "v" {
		t.Fatalf("%v %+v", err, c.Rows)
	}
	c = connector.Collector{}
	if err := r.Do(ctx, connector.Call{Verb: "scan", Args: []string{"tmvp:*"}, Limit: 5}, &c); err != nil || len(c.Rows) == 0 {
		t.Fatalf("scan %v", err)
	}
	_ = w.Do(ctx, connector.Call{Verb: "del", Args: []string{"tmvp:live"}}, &c)
}
