package ssh

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/pem"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

// dockerStub, when set, answers stdin lines that are docker commands
// (out, exit status, handled). Tests using it must not run in parallel.
var dockerStub func(line string) (string, uint32, bool)

// fakeServer is an in-process ssh server: exec echoes stdin back (one line per
// line), "direct-tcpip" forwards to the requested address.
type fakeServer struct {
	addr    string
	hostPub ssh.PublicKey
	ln      net.Listener
}

func genKey(t *testing.T) (ssh.Signer, []byte) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	blk, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s, pem.EncodeToMemory(blk)
}

func startServer(t *testing.T, clientPub ssh.PublicKey) *fakeServer {
	t.Helper()
	host, _ := genKey(t)
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
		if string(k.Marshal()) == string(clientPub.Marshal()) {
			return nil, nil
		}
		return nil, io.EOF
	}}
	cfg.AddHostKey(host)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go serveConn(nc, cfg)
		}
	}()
	return &fakeServer{addr: ln.Addr().String(), hostPub: host.PublicKey(), ln: ln}
}

func serveConn(nc net.Conn, cfg *ssh.ServerConfig) {
	_, chans, reqs, err := ssh.NewServerConn(nc, cfg)
	if err != nil {
		nc.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	for nch := range chans {
		switch nch.ChannelType() {
		case "session":
			ch, rq, _ := nch.Accept()
			go func() {
				for r := range rq {
					if r.Type != "exec" {
						r.Reply(r.Type == "signal", nil)
						continue
					}
					r.Reply(true, nil)
					go func() {
						sc := bufio.NewScanner(ch)
						status := uint32(0)
						for sc.Scan() {
							l := sc.Text()
							if strings.HasPrefix(l, "exit ") {
								status = 3
								break
							}
							if l == "sleep" {
								time.Sleep(5 * time.Second)
							}
							if stub := dockerStub; stub != nil {
								if out, st, ok := stub(l); ok {
									io.WriteString(ch, out)
									if st != 0 {
										status = st
										break
									}
									continue
								}
							}
							io.WriteString(ch, "out: "+l+"\n")
						}
						b := make([]byte, 4)
						binary.BigEndian.PutUint32(b, status)
						ch.SendRequest("exit-status", false, b)
						ch.Close()
					}()
				}
			}()
		case "direct-tcpip":
			var p struct {
				Host  string
				Port  uint32
				OHost string
				OPort uint32
			}
			ssh.Unmarshal(nch.ExtraData(), &p)
			up, err := net.Dial("tcp", net.JoinHostPort(p.Host, itoa(p.Port)))
			if err != nil {
				nch.Reject(ssh.ConnectionFailed, "nope")
				continue
			}
			ch, rq, _ := nch.Accept()
			go ssh.DiscardRequests(rq)
			go func() { io.Copy(ch, up); ch.CloseWrite() }()
			go func() { io.Copy(up, ch); up.Close() }()
		default:
			nch.Reject(ssh.UnknownChannelType, "no")
		}
	}
}

func itoa(p uint32) string { return strconv.FormatUint(uint64(p), 10) }

func target(t *testing.T, srv *fakeServer, keyPEM []byte, mode connector.Mode, extra map[string]any) *connector.Target {
	h, p, _ := net.SplitHostPort(srv.addr)
	f := connector.StaticFields{"host": h, "port": p, "user": "deploy", "key": string(keyPEM),
		"host_key": string(ssh.MarshalAuthorizedKey(srv.hostPub))}
	for k, v := range extra {
		if v == nil {
			delete(f, k)
		} else {
			f[k] = v
		}
	}
	return connector.NewTarget(connector.Target{System: "bastion", Mode: mode, Dir: t.TempDir()}, f)
}

func TestVerbTable(t *testing.T) {
	want := map[string]connector.VerbMode{"ls": connector.Read, "health": connector.Read, "docker": connector.Read, "script": connector.Read, "run": connector.Write}
	vs := New().Verbs()
	if len(vs) != len(want) {
		t.Fatalf("verbs %v", vs)
	}
	for _, v := range vs {
		if want[v.Name] != v.Mode {
			t.Errorf("%s mode %v", v.Name, v.Mode)
		}
	}
	if dv, _ := connector.DefaultWriteVerb(vs); dv.Name != "run" {
		t.Errorf("default write verb %q", dv.Name)
	}
}

func TestHealthScriptRun(t *testing.T) {
	cs, keyPEM := genKey(t)
	srv := startServer(t, cs.PublicKey())
	ctx := context.Background()

	tg := target(t, srv, keyPEM, connector.ModeRead, nil)
	os.MkdirAll(filepath.Join(tg.AssetsDir(), "scripts"), 0o755)
	os.WriteFile(filepath.Join(tg.AssetsDir(), "scripts", "disk.sh"), []byte("---\nread: true\n---\ndf -h\n"), 0o644)
	os.WriteFile(filepath.Join(tg.AssetsDir(), "scripts", "fix.sh"), []byte("---\ndescription: x\n---\nrm -rf /tmp/x\n"), 0o644)

	s := New().(*SSH)
	if err := s.Open(ctx, tg); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var c connector.Collector
	if err := s.Do(ctx, connector.Call{Verb: "health"}, &c); err != nil {
		t.Fatal(err)
	}
	if c.Rows[0]["ok"] != true {
		t.Fatalf("health %v", c.Rows)
	}
	c = connector.Collector{}
	if err := s.Do(ctx, connector.Call{Verb: "script", Args: []string{"@disk", "a b"}}, &c); err != nil {
		t.Fatal(err)
	}
	if strings.Join(c.Lines, "|") != "out: set -- 'a b'|out: df -h" {
		t.Fatalf("lines %q", c.Lines)
	}
	// undeclared script through read → policy_blocked
	if err := s.Do(ctx, connector.Call{Verb: "script", Args: []string{"@fix"}}, &c); errs.CodeOf(err) != errs.PolicyBlocked {
		t.Fatalf("want policy_blocked, got %v", err)
	}
	// ad-hoc command through read → policy_blocked (Guard, no connection)
	if err := New().(*SSH).Guard(connector.Call{Verb: "script", Args: []string{"rm -rf /"}}); errs.CodeOf(err) != errs.PolicyBlocked {
		t.Fatalf("guard: %v", err)
	}
	// run on a read-mode connector → policy_blocked
	if err := s.Do(ctx, connector.Call{Verb: "run", Args: []string{"touch x"}}, &c); errs.CodeOf(err) != errs.PolicyBlocked {
		t.Fatalf("run in read mode: %v", err)
	}

	w := New().(*SSH)
	if err := w.Open(ctx, target(t, srv, keyPEM, connector.ModeWrite, nil)); err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	c = connector.Collector{}
	if err := w.Do(ctx, connector.Call{Verb: "run", Args: []string{"echo hi"}}, &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Lines) != 1 || c.Lines[0] != "out: echo hi" {
		t.Fatalf("run %q", c.Lines)
	}
	if err := w.Do(ctx, connector.Call{Verb: "run", Args: []string{"exit 3"}}, &c); errs.CodeOf(err) != errs.Runtime {
		t.Fatalf("exit status: %v", err)
	}
	c = connector.Collector{}
	if err := w.Do(ctx, connector.Call{Verb: "run", Args: []string{"x"}, Preview: true}, &c); err != nil || c.Rows[0]["preview"] != true {
		t.Fatalf("preview %v %v", err, c.Rows)
	}
}

func TestLineCapAndTimeout(t *testing.T) {
	cs, keyPEM := genKey(t)
	srv := startServer(t, cs.PublicKey())
	ctx := context.Background()
	tg := target(t, srv, keyPEM, connector.ModeWrite, nil)
	tg.Limits.Lines = 3
	tg.Limits.Timeout = 500 * time.Millisecond
	s := New().(*SSH)
	if err := s.Open(ctx, tg); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var c connector.Collector
	if err := s.Do(ctx, connector.Call{Verb: "run", Args: []string{"1\n2\n3\n4\n5\n6"}}, &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Lines) != 4 || !strings.Contains(c.Lines[3], "truncated") {
		t.Fatalf("cap: %q", c.Lines)
	}
	start := time.Now()
	err := s.Do(ctx, connector.Call{Verb: "run", Args: []string{"sleep"}}, &c)
	if errs.CodeOf(err) != errs.Timeout || time.Since(start) > 3*time.Second {
		t.Fatalf("timeout: %v after %s", err, time.Since(start))
	}
}

func TestAuthAndHostKeyErrors(t *testing.T) {
	cs, _ := genKey(t)
	_, otherPEM := genKey(t)
	srv := startServer(t, cs.PublicKey())
	ctx := context.Background()

	err := New().Open(ctx, target(t, srv, otherPEM, connector.ModeRead, nil))
	if errs.CodeOf(err) != errs.Auth {
		t.Fatalf("want auth_error, got %v", err)
	}
	wrongHost, _ := genKey(t)
	_, cPEM := genKey(t)
	err = New().Open(ctx, target(t, srv, cPEM, connector.ModeRead, map[string]any{"host_key": string(ssh.MarshalAuthorizedKey(wrongHost.PublicKey()))}))
	if errs.CodeOf(err) != errs.PolicyBlocked {
		t.Fatalf("want host key mismatch, got %v", err)
	}
	err = New().Open(ctx, target(t, srv, cPEM, connector.ModeRead, map[string]any{"host_key": nil}))
	if errs.CodeOf(err) != errs.Config {
		t.Fatalf("want config error without host key, got %v", err)
	}
	err = New().Open(ctx, target(t, srv, []byte("FAKE_SECRET_123"), connector.ModeRead, nil))
	if errs.CodeOf(err) != errs.Config || strings.Contains(err.Error(), "FAKE_SECRET_123") {
		t.Fatalf("bad key: %v", err)
	}
}

func TestKnownHosts(t *testing.T) {
	cs, keyPEM := genKey(t)
	srv := startServer(t, cs.PublicKey())
	h, p, _ := net.SplitHostPort(srv.addr)
	line := "[" + h + "]:" + p + " " + string(ssh.MarshalAuthorizedKey(srv.hostPub))
	s := New()
	if err := s.Open(context.Background(), target(t, srv, keyPEM, connector.ModeRead, map[string]any{"host_key": nil, "known_hosts": line})); err != nil {
		t.Fatal(err)
	}
	s.Close()
	other := "otherhost " + string(ssh.MarshalAuthorizedKey(srv.hostPub))
	err := New().Open(context.Background(), target(t, srv, keyPEM, connector.ModeRead, map[string]any{"host_key": nil, "known_hosts": other}))
	if errs.CodeOf(err) != errs.Config {
		t.Fatalf("unknown host: %v", err)
	}
	if !hashedMatch(knownhosts.HashHostname("[h]:2222"), "[h]:2222") || hashedMatch(knownhosts.HashHostname("x"), "y") {
		t.Fatal("hashed match")
	}
}

// TestTunnelChain dials an echo server through two ssh hops (bastion → jump2).
func TestTunnelChain(t *testing.T) {
	cs, keyPEM := genKey(t)
	srv1 := startServer(t, cs.PublicKey())
	srv2 := startServer(t, cs.PublicKey())
	echo, _ := net.Listen("tcp", "127.0.0.1:0")
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	ctx := context.Background()
	t1, err := Tunneler{}.OpenTunnel(ctx, target(t, srv1, keyPEM, connector.ModeRead, nil), connector.DirectDialer())
	if err != nil {
		t.Fatal(err)
	}
	defer t1.Close()
	t2, err := Tunneler{}.OpenTunnel(ctx, target(t, srv2, keyPEM, connector.ModeRead, nil), t1)
	if err != nil {
		t.Fatal(err)
	}
	defer t2.Close()
	c, err := t2.DialContext(ctx, "tcp", echo.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c.Write([]byte("ping\n"))
	got, _ := bufio.NewReader(c).ReadString('\n')
	if got != "ping\n" {
		t.Fatalf("echo %q", got)
	}
	c.Close()
	if _, ok := connector.LookupTunneler("ssh"); !ok {
		t.Fatal("ssh tunneler not registered")
	}
}

func TestLiveSSH(t *testing.T) {
	host := os.Getenv("TMVP_TEST_SSH_HOST")
	if host == "" {
		t.Skip("TMVP_TEST_SSH_HOST not set")
	}
	f := connector.StaticFields{"host": host, "user": os.Getenv("TMVP_TEST_SSH_USER"), "key": os.Getenv("TMVP_TEST_SSH_KEY"),
		"known_hosts": os.Getenv("TMVP_TEST_SSH_KNOWN_HOSTS")}
	s := New()
	if err := s.Open(context.Background(), connector.NewTarget(connector.Target{Mode: connector.ModeRead}, f)); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var c connector.Collector
	if err := s.Do(context.Background(), connector.Call{Verb: "health"}, &c); err != nil {
		t.Fatal(err)
	}
}
