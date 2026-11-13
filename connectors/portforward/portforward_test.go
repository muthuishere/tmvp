package portforward

import (
	"bufio"
	"context"
	"io"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

// recDialer dials a local echo server, recording what was asked for.
type recDialer struct {
	mu    sync.Mutex
	asked []string
	echo  string
}

func (r *recDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	r.mu.Lock()
	r.asked = append(r.asked, network+" "+addr)
	r.mu.Unlock()
	return net.Dial("tcp", r.echo)
}

func echoServer(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	return ln.Addr().String()
}

func TestVerbTable(t *testing.T) {
	for _, v := range New().Verbs() {
		if v.Mode != connector.Read {
			t.Errorf("%s should be read", v.Name)
		}
	}
	if len(New().Verbs()) != 4 {
		t.Fatal("verbs")
	}
	if _, ok := connector.LookupTunneler("portforward"); !ok {
		t.Fatal("tunneler not registered")
	}
}

func TestTunnelIgnoresAddr(t *testing.T) {
	d := &recDialer{echo: echoServer(t)}
	hop := connector.NewTarget(connector.Target{Mode: connector.ModeRead}, connector.StaticFields{"remote": "db.internal:5432"})
	tun, err := Tunneler{}.OpenTunnel(context.Background(), hop, d)
	if err != nil {
		t.Fatal(err)
	}
	c, err := tun.DialContext(context.Background(), "tcp", "whatever:1")
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if strings.Join(d.asked, ",") != "tcp db.internal:5432,tcp db.internal:5432" {
		t.Fatalf("asked %v", d.asked)
	}
	hop2 := connector.NewTarget(connector.Target{Mode: connector.ModeRead}, connector.StaticFields{"target": "svc/postgres:5432", "namespace": "data"})
	d.asked = nil
	if _, err := (Tunneler{}).OpenTunnel(context.Background(), hop2, d); err != nil {
		t.Fatal(err)
	}
	if d.asked[0] != "k8s data/svc/postgres:5432" {
		t.Fatalf("k8s target %v", d.asked)
	}
}

func TestConfigErrors(t *testing.T) {
	for _, f := range []connector.StaticFields{{}, {"remote": "nohost"}, {"target": "postgres:5432"}} {
		hop := connector.NewTarget(connector.Target{}, f)
		if _, err := (Tunneler{}).OpenTunnel(context.Background(), hop, &recDialer{}); errs.CodeOf(err) != errs.Config {
			t.Errorf("%v: %v", f, err)
		}
	}
}

func TestStatusOpenClose(t *testing.T) {
	d := &recDialer{echo: echoServer(t)}
	tg := connector.NewTarget(connector.Target{Product: "acme", Env: "prod", System: "db-tunnel", Mode: connector.ModeRead, Via: []string{"bastion"}, Dialer: d},
		connector.StaticFields{"remote": "db.internal:5432"})
	p := New()
	ctx := context.Background()
	if err := p.Open(ctx, tg); err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	var c connector.Collector
	if err := p.Do(ctx, connector.Call{Verb: "status"}, &c); err != nil || c.Rows[0]["ok"] != true {
		t.Fatalf("status %v %v", err, c.Rows)
	}
	if err := p.Do(ctx, connector.Call{Verb: "open"}, &c); err != nil {
		t.Fatal(err)
	}
	addr := c.Rows[1]["listener"].(string)
	// default port = the convention `tmvp export pg-service` prints
	if want := "127.0.0.1:" + connector.LocalPortString("acme", "prod", "db-tunnel"); addr != want {
		t.Fatalf("listener %q want %q", addr, want)
	}
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	conn.Write([]byte("hi\n"))
	if l, _ := bufio.NewReader(conn).ReadString('\n'); l != "hi\n" {
		t.Fatalf("echo %q", l)
	}
	conn.Close()
	if err := p.Do(ctx, connector.Call{Verb: "close"}, &c); err != nil || c.Rows[2]["closed"] != true {
		t.Fatalf("close %v %v", err, c.Rows)
	}
	if _, err := net.Dial("tcp", addr); err == nil {
		t.Fatal("listener still open")
	}
}
