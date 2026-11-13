package server

// End-to-end via: chains through the real ssh, k8s and portforward tunnelers
// and the real http connector, against an in-process ssh server and a fake
// Kubernetes API (connectors/k8s/k8stest) — no real cluster.

import (
	"bufio"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	_ "github.com/muthuishere/tmvp/connectors/http"
	_ "github.com/muthuishere/tmvp/connectors/k8s"
	"github.com/muthuishere/tmvp/connectors/k8s/k8stest"
	_ "github.com/muthuishere/tmvp/connectors/portforward"
	_ "github.com/muthuishere/tmvp/connectors/ssh"
	"github.com/muthuishere/tmvp/internal/errs"
	"github.com/muthuishere/tmvp/internal/protocol"
)

// sshJump is a minimal ssh server that only forwards direct-tcpip channels.
type sshJump struct {
	addr     string
	hostLine string // authorized_keys line of the host key
	clientPm []byte // client private key (PEM)
	handshakes,
	channels atomic.Int32
}

func startJump(t *testing.T) *sshJump {
	t.Helper()
	key := func() (ssh.Signer, []byte) {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		blk, err := ssh.MarshalPrivateKey(priv, "")
		if err != nil {
			t.Fatal(err)
		}
		s, _ := ssh.NewSignerFromKey(priv)
		return s, pem.EncodeToMemory(blk)
	}
	host, _ := key()
	client, clientPem := key()
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
		if string(k.Marshal()) == string(client.PublicKey().Marshal()) {
			return nil, nil
		}
		return nil, io.EOF
	}}
	cfg.AddHostKey(host)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	j := &sshJump{addr: ln.Addr().String(), hostLine: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(host.PublicKey()))), clientPm: clientPem}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_, chans, reqs, err := ssh.NewServerConn(nc, cfg)
				if err != nil {
					nc.Close()
					return
				}
				j.handshakes.Add(1)
				go ssh.DiscardRequests(reqs)
				for nch := range chans {
					if nch.ChannelType() != "direct-tcpip" {
						nch.Reject(ssh.UnknownChannelType, "no")
						continue
					}
					var p struct {
						Host  string
						Port  uint32
						OHost string
						OPort uint32
					}
					_ = ssh.Unmarshal(nch.ExtraData(), &p)
					up, err := net.Dial("tcp", net.JoinHostPort(p.Host, strconv.Itoa(int(p.Port))))
					if err != nil {
						nch.Reject(ssh.ConnectionFailed, "refused")
						continue
					}
					ch, rq, _ := nch.Accept()
					j.channels.Add(1)
					go ssh.DiscardRequests(rq)
					go func() { io.Copy(ch, up); ch.CloseWrite() }()
					go func() { io.Copy(up, ch); up.Close() }()
				}
			}()
		}
	}()
	return j
}

const fwdCfg = `
version: 1
product: fwd
logging: { path: ./logs }
envs:
  prod:
    secrets: { kc: "$(get kc)", sshkey: "$(get sshkey)", hostkey: "$(get hostkey)" }
    systems:
      bastion:     { type: ssh, host: 127.0.0.1, port: %s, user: deploy, key: "${secrets.sshkey}", host_key: "${secrets.hostkey}" }
      cluster:     { type: k8s, kubeconfig: "${secrets.kc}", context: prod }
      cluster-ssh: { type: k8s, via: bastion, kubeconfig: "${secrets.kc}", context: prod }
      web-fwd:     { type: portforward, via: cluster, target: deploy/web:http }
      web-fwd3:    { type: portforward, via: cluster-ssh, target: svc/web:80 }
      cold-fwd:    { type: portforward, via: cluster, target: deploy/cold:80 }
      web:         { type: http, via: web-fwd, base_url: "http://web.internal" }
      web3:        { type: http, via: web-fwd3, base_url: "http://web.internal" }
      cold:        { type: http, via: cold-fwd, base_url: "http://cold.internal" }
`

// httpPod answers every HTTP request on a forwarded stream with the pod name.
func httpPod(pod string, port int, rw io.ReadWriter) {
	br := bufio.NewReader(rw)
	for {
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, req.Body)
		body := fmt.Sprintf(`{"pod":%q,"port":%d,"path":%q}`, pod, port, req.URL.Path)
		fmt.Fprintf(rw, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
	}
}

type fwdEnv struct {
	e    *Engine
	cfg  string
	api  *k8stest.API
	jump *sshJump
}

func newFwdEnv(t *testing.T) *fwdEnv {
	t.Helper()
	api := k8stest.New(t)
	web := map[string]string{"app": "web"}
	api.AddPod(k8stest.Pod{Name: "web-0", Labels: web, Running: true, Ports: map[string]int{"http": 8080}}) // not ready
	api.AddPod(k8stest.Pod{Name: "web-1", Labels: web, Running: true, Ready: true, Ports: map[string]int{"http": 8080}})
	api.AddPod(k8stest.Pod{Name: "cold-0", Labels: map[string]string{"app": "cold"}})
	api.AddDeployment("web", web)
	api.AddDeployment("cold", map[string]string{"app": "cold"})
	api.AddService("web", web, 80, "http")
	api.SetHandler(httpPod)
	jump := startJump(t)
	s := &secrets{m: map[string]string{"get kc": api.Kubeconfig(), "get sshkey": string(jump.clientPm), "get hostkey": jump.hostLine}, ran: map[string]int{}}
	e := newEngine(s)
	t.Cleanup(e.Shutdown)
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	_, port, _ := net.SplitHostPort(jump.addr)
	if err := os.WriteFile(p, []byte(fmt.Sprintf(fwdCfg, port)), 0o600); err != nil {
		t.Fatal(err)
	}
	return &fwdEnv{e: e, cfg: p, api: api, jump: jump}
}

func (f *fwdEnv) run(t *testing.T, sys, verb string, args ...string) result {
	t.Helper()
	return call(t, f.e, protocol.Request{Op: protocol.OpRun, ConfigPath: f.cfg, Env: "prod", System: sys, Verb: verb, Args: args})
}

func (f *fwdEnv) tunnelKeys() []string {
	var out []string
	f.e.mu.Lock()
	defer f.e.mu.Unlock()
	for _, p := range f.e.products {
		p.mu.Lock()
		for k := range p.tunnels {
			out = append(out, k)
		}
		p.mu.Unlock()
	}
	return out
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for i := 0; i < 300; i++ {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// (a) http system via portforward via k8s: deploy/<name> resolves to the
// Ready pod, bytes flow both ways, connections close; no ready pod is a
// clear not_found.
func TestViaK8sPortForwardHTTP(t *testing.T) {
	f := newFwdEnv(t)
	for i := 0; i < 3; i++ {
		r := f.run(t, "web", "get", fmt.Sprintf("/hello/%d", i))
		if r.exit != 0 {
			t.Fatalf("exit %d: %s", r.exit, r.raw)
		}
		if !strings.Contains(r.raw, `"pod":"web-1"`) || !strings.Contains(r.raw, fmt.Sprintf(`/hello/%d`, i)) || !strings.Contains(r.raw, `8080`) {
			t.Fatalf("want web-1:8080: %s", r.raw)
		}
	}
	r := f.run(t, "web-fwd", "status")
	if r.exit != 0 || r.of(protocol.FrameRow)[0].Row["ok"] != true {
		t.Fatalf("status: %s", r.raw)
	}
	r = f.run(t, "cold", "get", "/")
	if r.exit == 0 || r.of(protocol.FrameError)[0].Error.Code != errs.NotFound || !strings.Contains(r.raw, "no ready pod") {
		t.Fatalf("cold: %s", r.raw)
	}
	// every forward ends when its call ends (http keep-alive is per connector,
	// so allow it to be idle-pooled; none of them may outlive Shutdown)
	f.e.Shutdown()
	waitFor(t, "forwards to close", func() bool { return f.api.Active.Load() == 0 && f.api.Conns.Load() == 0 })
	for _, req := range f.api.Requests() {
		if !strings.HasPrefix(req, "GET ") && !strings.HasSuffix(req, "/pods/web-1/portforward") {
			t.Errorf("unexpected API request %s", req)
		}
	}
}

// (a) raw TCP: a portforward system's local listener, many concurrent
// clients, both directions, through the k8s hop.
func TestViaK8sPortForwardListenerConcurrent(t *testing.T) {
	f := newFwdEnv(t)
	r := f.run(t, "web-fwd", "open")
	if r.exit != 0 {
		t.Fatalf("open: %s", r.raw)
	}
	addr, _ := r.of(protocol.FrameRow)[0].Row["listener"].(string)
	var wg sync.WaitGroup
	errc := make(chan error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := net.Dial("tcp", addr)
			if err != nil {
				errc <- err
				return
			}
			defer c.Close()
			br := bufio.NewReader(c)
			for j := 0; j < 5; j++ {
				fmt.Fprintf(c, "GET /c%d/r%d HTTP/1.1\r\nHost: x\r\n\r\n", i, j)
				resp, err := http.ReadResponse(br, nil)
				if err != nil {
					errc <- err
					return
				}
				b, _ := io.ReadAll(resp.Body)
				if !strings.Contains(string(b), fmt.Sprintf(`"/c%d/r%d"`, i, j)) || !strings.Contains(string(b), "web-1") {
					errc <- fmt.Errorf("conn %d: %s", i, b)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errc)
	for err := range errc {
		t.Error(err)
	}
	waitFor(t, "client-closed forwards to end", func() bool { return f.api.Active.Load() == 0 && f.api.Conns.Load() == 0 })
	if r := f.run(t, "web-fwd", "close"); r.exit != 0 || r.of(protocol.FrameRow)[0].Row["closed"] != true {
		t.Fatalf("close: %s", r.raw)
	}
	if _, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		t.Fatal("listener still open after close")
	}
}

// (b) a k8s system with via: <ssh> reaches the API through the ssh tunnel.
func TestK8sViaSSH(t *testing.T) {
	f := newFwdEnv(t)
	r := f.run(t, "cluster-ssh", "get", "pods")
	if r.exit != 0 || !strings.Contains(r.raw, "web-1") {
		t.Fatalf("exit %d: %s", r.exit, r.raw)
	}
	if f.jump.handshakes.Load() != 1 || f.jump.channels.Load() == 0 {
		t.Fatalf("ssh handshakes %d channels %d", f.jump.handshakes.Load(), f.jump.channels.Load())
	}
}

// (c) ssh -> k8s -> pod: http via portforward(svc/web) via k8s via ssh, and
// tunnels are shared: one ssh connection, one k8s tunnel per chain prefix.
func TestSSHK8sPodThreeHops(t *testing.T) {
	f := newFwdEnv(t)
	base := runtime.NumGoroutine()
	for i := 0; i < 4; i++ {
		r := f.run(t, "web3", "get", "/three")
		if r.exit != 0 || !strings.Contains(r.raw, `"pod":"web-1"`) || !strings.Contains(r.raw, `"port":8080`) {
			t.Fatalf("exit %d: %s", r.exit, r.raw)
		}
	}
	if r := f.run(t, "cluster-ssh", "get", "pods"); r.exit != 0 {
		t.Fatalf("%s", r.raw)
	}
	if h := f.jump.handshakes.Load(); h != 1 {
		t.Fatalf("ssh tunnel not reused: %d handshakes", h)
	}
	want := map[string]bool{"prod/bastion": true, "prod/bastion>cluster-ssh": true, "prod/bastion>cluster-ssh>web-fwd3": true}
	got := f.tunnelKeys()
	for _, k := range got {
		delete(want, k)
	}
	if len(want) != 0 || len(got) != 3 {
		t.Fatalf("tunnels %v (missing %v)", got, want)
	}
	// (e) shutdown closes every tunnel and forward; nothing is left running
	f.e.Shutdown()
	waitFor(t, "forwards to close", func() bool { return f.api.Active.Load() == 0 && f.api.Conns.Load() == 0 })
	if len(f.tunnelKeys()) != 0 {
		t.Fatalf("tunnels left: %v", f.tunnelKeys())
	}
	waitFor(t, "goroutines to exit", func() bool { return runtime.NumGoroutine() <= base })
}

// (e) a call whose deadline expires while dialing the forward fails with a
// timeout and leaves no forward behind.
func TestViaK8sTimeoutTearsDown(t *testing.T) {
	f := newFwdEnv(t)
	// the pod never answers; its stream ends only when tmvp tears the forward down
	f.api.SetHandler(func(pod string, port int, rw io.ReadWriter) { _, _ = io.Copy(io.Discard, rw) })
	r := call(t, f.e, protocol.Request{Op: protocol.OpRun, ConfigPath: f.cfg, Env: "prod", System: "web", Verb: "get", Args: []string{"/slow"}, Timeout: "1s"})
	if r.exit == 0 || r.of(protocol.FrameError)[0].Error.Code != errs.Timeout {
		t.Fatalf("slow call: %s", r.raw)
	}
	waitFor(t, "forward to close after timeout", func() bool { return f.api.Active.Load() == 0 && f.api.Conns.Load() == 0 })
}

// A hot reload closes every tunnel; a conn that dials through a via chain
// must be reopened even when its own config did not change.
func TestReloadReopensViaConns(t *testing.T) {
	f := newFwdEnv(t)
	if r := f.run(t, "web3", "get", "/before"); r.exit != 0 {
		t.Fatalf("before: %s", r.raw)
	}
	b, err := os.ReadFile(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.cfg, append(b, []byte("\n# unrelated edit\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := f.run(t, "web3", "get", "/after"); r.exit != 0 || !strings.Contains(r.raw, `"pod":"web-1"`) {
		t.Fatalf("after reload: exit %d: %s", r.exit, r.raw)
	}
}

// The audit record names the full via chain the call went through.
func TestAuditRecordsViaChain(t *testing.T) {
	f := newFwdEnv(t)
	if r := f.run(t, "web3", "get", "/a"); r.exit != 0 {
		t.Fatalf("%s", r.raw)
	}
	var found bool
	_ = filepath.Walk(filepath.Join(filepath.Dir(f.cfg), "logs"), func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			b, _ := os.ReadFile(p)
			// encoding/json escapes '>' as \u003e
			found = found || strings.Contains(string(b), `"via":"bastion\u003ecluster-ssh\u003eweb-fwd3"`)
		}
		return nil
	})
	if !found {
		t.Fatal("audit log has no via chain")
	}
}
