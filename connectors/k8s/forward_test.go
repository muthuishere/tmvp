package k8s

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/muthuishere/tmvp/connectors/k8s/k8stest"
	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

// fwdAPI: deploy/web and svc/db select app=web; only web-3 is Running+Ready
// and not terminating. svc/empty selects pods that are all unready.
func fwdAPI(t *testing.T) *k8stest.API {
	a := k8stest.New(t)
	web := map[string]string{"app": "web"}
	pg := map[string]int{"pg": 5432}
	a.AddPod(k8stest.Pod{Name: "web-0", Labels: web, Ports: pg})                // Pending
	a.AddPod(k8stest.Pod{Name: "web-1", Labels: web, Running: true, Ports: pg}) // not Ready
	a.AddPod(k8stest.Pod{Name: "web-2", Labels: web, Running: true, Ready: true, Terminating: true, Ports: pg})
	a.AddPod(k8stest.Pod{Name: "web-3", Labels: web, Running: true, Ready: true, Ports: pg})
	a.AddPod(k8stest.Pod{Name: "cold-0", Labels: map[string]string{"app": "cold"}, Running: true})
	a.AddDeployment("web", web)
	a.AddDeployment("cold", map[string]string{"app": "cold"})
	a.AddService("db", web, 5432, "pg")
	a.AddService("empty", map[string]string{"app": "cold"}, 80, 8080)
	a.AddService("none", map[string]string{"app": "ghost"}, 80, 8080)
	return a
}

// openFwd opens the tunneler through a counting non-direct parent dialer.
func openFwd(t *testing.T, a *k8stest.API) (*Tunnel, *atomic.Int32) {
	t.Helper()
	hop := connector.NewTarget(connector.Target{System: "cluster", Mode: connector.ModeRead},
		connector.StaticFields{"kubeconfig": a.Kubeconfig(), "context": "prod"})
	var dials atomic.Int32
	parent := connector.DialerFunc(func(ctx context.Context, network, addr string) (net.Conn, error) {
		dials.Add(1)
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	})
	tun, err := Tunneler{}.OpenTunnel(context.Background(), hop, parent)
	if err != nil {
		t.Fatal(err)
	}
	return tun.(*Tunnel), &dials
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for i := 0; i < 300; i++ {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	buf := make([]byte, 1<<20)
	t.Fatalf("timed out waiting for %s\n%s", what, buf[:runtime.Stack(buf, true)])
}

func roundTrip(c net.Conn, msg string) error {
	if _, err := io.WriteString(c, msg); err != nil {
		return err
	}
	b := make([]byte, len(msg))
	if _, err := io.ReadFull(c, b); err != nil {
		return err
	}
	if string(b) != strings.ToUpper(msg) {
		return fmt.Errorf("got %q", b)
	}
	return nil
}

func TestForwardResolvesReadyPod(t *testing.T) {
	a := fwdAPI(t)
	tun, dials := openFwd(t, a)
	defer tun.Close()
	for addr, want := range map[string]string{
		"svc/db:5432":        "web-3", // service port -> named targetPort -> ready pod
		"svc/db:pg":          "web-3",
		"app/deploy/web:pg":  "web-3",
		"deploy/web:5432":    "web-3",
		"deployment/web:pg":  "web-3",
		"pod/web-1:5432":     "web-1", // an explicit pod is used as named
		"app/pods/web-3:pg":  "web-3",
		"po/web-3:5432":      "web-3",
		"deploy.apps/web:pg": "web-3",
	} {
		ns, pod, port, err := tun.Resolve(context.Background(), addr)
		if err != nil || ns != "app" || pod != want || port != 5432 {
			t.Errorf("%s: %s/%s:%d %v (want %s:5432)", addr, ns, pod, port, err, want)
		}
	}
	c, err := tun.DialContext(context.Background(), Network, "deploy/web:pg")
	if err != nil {
		t.Fatal(err)
	}
	if err := roundTrip(c, "select 1"); err != nil {
		t.Fatal(err)
	}
	c.Close()
	if f := a.Forwarded(); len(f) != 1 || f[0] != "web-3" {
		t.Fatalf("forwarded to %v", f)
	}
	if dials.Load() == 0 {
		t.Fatal("API traffic bypassed the via dialer")
	}
}

func TestForwardNoReadyPod(t *testing.T) {
	a := fwdAPI(t)
	tun, _ := openFwd(t, a)
	defer tun.Close()
	for _, addr := range []string{"deploy/cold:80", "svc/empty:80"} {
		_, err := tun.DialContext(context.Background(), Network, addr)
		if errs.CodeOf(err) != errs.NotFound || !strings.Contains(err.Error(), "no ready pod matches app=cold") {
			t.Errorf("%s: %v", addr, err)
		}
	}
	for addr, code := range map[string]errs.Code{
		"svc/none:80":       errs.NotFound, // selector matches nothing
		"svc/missing:80":    errs.NotFound,
		"deploy/missing:80": errs.NotFound,
		"pod/web-3:nope":    errs.NotFound, // no such named port
		"widgets/x:80":      errs.NotFound, // no such kind
		"db.internal:5432":  errs.Unsupported,
		"a/b/c/d:1":         errs.Usage,
	} {
		if _, err := tun.DialContext(context.Background(), Network, addr); errs.CodeOf(err) != code {
			t.Errorf("%s: %v (want %s)", addr, err, code)
		}
	}
	if len(a.Forwarded()) != 0 {
		t.Fatalf("forwarded despite errors: %v", a.Forwarded())
	}
}

// Many concurrent connections, each with its own bytes, both directions.
func TestForwardConcurrentBidirectional(t *testing.T) {
	a := fwdAPI(t)
	base := runtime.NumGoroutine()
	tun, _ := openFwd(t, a)
	defer tun.Close()
	const n = 12
	var wg sync.WaitGroup
	errc := make(chan error, n)
	conns := make(chan net.Conn, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			addr := []string{"svc/db:5432", "deploy/web:pg", "pod/web-3:5432"}[i%3]
			c, err := tun.DialContext(context.Background(), Network, addr)
			if err != nil {
				errc <- err
				return
			}
			conns <- c
			for j := 0; j < 20; j++ {
				if err := roundTrip(c, fmt.Sprintf("conn %d msg %d;", i, j)); err != nil {
					errc <- fmt.Errorf("conn %d: %w", i, err)
					return
				}
			}
			// a larger payload, written while reading (full duplex)
			big := bytes.Repeat([]byte("abcdefgh"), 32<<10)
			go func() { _, _ = c.Write(big) }()
			got := make([]byte, len(big))
			if _, err := io.ReadFull(c, got); err != nil || !bytes.Equal(got, bytes.ToUpper(big)) {
				errc <- fmt.Errorf("conn %d big: %v", i, err)
			}
		}()
	}
	wg.Wait()
	close(errc)
	for err := range errc {
		t.Error(err)
	}
	if a.Active.Load() != n || tun.Live() != n {
		t.Fatalf("server streams %d, tunnel live %d, want %d", a.Active.Load(), tun.Live(), n)
	}
	close(conns)
	for c := range conns {
		c.Close()
	}
	// closing the client side ends the server stream and the SPDY connection
	eventually(t, "server streams to close", func() bool { return a.Active.Load() == 0 && a.Conns.Load() == 0 })
	if tun.Live() != 0 {
		t.Fatalf("tunnel still tracks %d conns", tun.Live())
	}
	// closing the tunnel releases the API client's pooled connections too
	tun.Close()
	eventually(t, "client goroutines to exit", func() bool { return runtime.NumGoroutine() <= base })
}

// The pod side closing (EOF) reaches the client; a refused port ends the conn.
func TestForwardRemoteCloseAndRefused(t *testing.T) {
	a := fwdAPI(t)
	a.SetHandler(func(_ string, _ int, rw io.ReadWriter) { _, _ = io.WriteString(rw, "bye") })
	tun, _ := openFwd(t, a)
	defer tun.Close()
	c, err := tun.DialContext(context.Background(), Network, "pod/web-3:5432")
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	b, err := io.ReadAll(c)
	if string(b) != "bye" || err != nil {
		t.Fatalf("%q %v", b, err)
	}
	c.Close()
	c, err = tun.DialContext(context.Background(), Network, "pod/web-3:9") // nothing listening
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil || isTimeout(err) {
		t.Fatalf("refused port: read err %v", err)
	}
	c.Close()
	eventually(t, "spdy conns to close", func() bool { return a.Conns.Load() == 0 })
}

func isTimeout(err error) bool {
	ne, ok := err.(net.Error)
	return ok && ne.Timeout()
}

// A cancelled dial fails fast; Tunnel.Close tears down live forwards.
func TestForwardCancelAndTunnelClose(t *testing.T) {
	a := fwdAPI(t)
	tun, _ := openFwd(t, a)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := tun.DialContext(ctx, Network, "pod/web-3:5432"); err == nil {
		t.Fatal("cancelled dial succeeded")
	}
	base := runtime.NumGoroutine()
	var cs []net.Conn
	for i := 0; i < 3; i++ {
		c, err := tun.DialContext(context.Background(), Network, "svc/db:5432")
		if err != nil {
			t.Fatal(err)
		}
		if err := roundTrip(c, "hi"); err != nil {
			t.Fatal(err)
		}
		cs = append(cs, c)
	}
	if err := tun.Close(); err != nil {
		t.Fatal(err)
	}
	for _, c := range cs {
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := c.Read(make([]byte, 1)); err == nil || isTimeout(err) {
			t.Fatalf("conn survived tunnel close: %v", err)
		}
	}
	eventually(t, "server side to tear down", func() bool { return a.Active.Load() == 0 && a.Conns.Load() == 0 })
	eventually(t, "goroutines to exit", func() bool { return runtime.NumGoroutine() <= base })
	if _, err := tun.DialContext(context.Background(), Network, "svc/db:5432"); errs.CodeOf(err) != errs.Unreachable {
		t.Fatalf("dial after close: %v", err)
	}
}

// Read mode: only the tunneler's client may make the port-forward POST
// upgrade, and only to the pods/<name>/portforward subresource.
func TestReadGuardPortForwardOnlyForTunnel(t *testing.T) {
	a := fwdAPI(t)
	plain := New().(*K8s)
	tg := connector.NewTarget(connector.Target{System: "cluster", Mode: connector.ModeRead, Dir: t.TempDir()},
		connector.StaticFields{"kubeconfig": a.Kubeconfig(), "context": "prod"})
	if err := plain.Open(context.Background(), tg); err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	if _, err := plain.c.portForward(context.Background(), "app", "web-3", 5432); errs.CodeOf(err) != errs.PolicyBlocked {
		t.Fatalf("read connector port-forward: %v", err)
	}
	tun, _ := openFwd(t, a)
	defer tun.Close()
	for _, p := range []string{"/api/v1/namespaces/app/pods", "/api/v1/namespaces/app/pods/web-3/exec",
		"/api/v1/namespaces/app/pods/web-3/portforward", "/apis/x.io/v1/portforward"} {
		for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
			if err := tun.k.c.do(context.Background(), m, p, nil, []byte("{}"), "application/json", nil); errs.CodeOf(err) != errs.PolicyBlocked {
				t.Errorf("tunnel client %s %s: %v", m, p, err)
			}
		}
	}
	for _, r := range a.Requests() {
		if !strings.HasPrefix(r, "GET ") && r != "POST /api/v1/namespaces/app/pods/web-3/portforward" {
			t.Errorf("non-GET reached the API: %s", r)
		}
	}
	if c, err := tun.DialContext(context.Background(), Network, "pod/web-3:5432"); err != nil {
		t.Fatal(err)
	} else {
		c.Close()
	}
}
