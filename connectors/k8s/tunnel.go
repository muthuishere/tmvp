package k8s

import (
	"context"
	"encoding/json"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

// Network is the dial network a k8s tunnel understands. A portforward hop
// with `target: svc/postgres:5432` dials Network "[ns/]svc/postgres:5432".
const Network = "k8s"

// Tunneler is the k8s port-forward hop (used by `type: portforward, via: <k8s system>`).
type Tunneler struct{}

// OpenTunnel builds an API client for the hop (API traffic goes through parent).
func (Tunneler) OpenTunnel(ctx context.Context, hop *connector.Target, parent connector.Dialer) (connector.Tunnel, error) {
	hop.Dialer = parent
	// hop.Mode is read: the read transport still allows the port-forward upgrade.
	k := &K8s{allowPortForward: true}
	if err := k.Open(ctx, hop); err != nil {
		return nil, err
	}
	return &Tunnel{k: k, live: map[*fwdConn]struct{}{}}, nil
}

// Tunnel port-forwards to pods through the Kubernetes API. It is shared by
// every system routed through it; Close tears down every live forward.
type Tunnel struct {
	k *K8s

	mu     sync.Mutex
	closed bool
	live   map[*fwdConn]struct{}
}

// fwdConn deregisters itself from its tunnel on Close.
type fwdConn struct {
	net.Conn
	t    *Tunnel
	once sync.Once
}

func (c *fwdConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() {
		c.t.mu.Lock()
		delete(c.t.live, c)
		c.t.mu.Unlock()
	})
	return err
}

// DialContext dials "[ns/]kind/name:port" (kind: pod|svc|deploy|sts|...).
// Plain host:port is not reachable through a k8s hop.
func (t *Tunnel) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if t.isClosed() {
		return nil, errs.New(errs.Unreachable, "k8s tunnel is closed")
	}
	ns, pod, port, err := t.Resolve(ctx, addr)
	if err != nil {
		return nil, err
	}
	c, err := t.k.c.portForward(ctx, ns, pod, port)
	if err != nil {
		return nil, err
	}
	fc := &fwdConn{Conn: c, t: t}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		_ = c.Close()
		return nil, errs.New(errs.Unreachable, "k8s tunnel is closed")
	}
	t.live[fc] = struct{}{}
	t.mu.Unlock()
	return fc, nil
}

func (t *Tunnel) isClosed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closed
}

// Live is the number of open forwarded connections.
func (t *Tunnel) Live() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.live)
}

// Close closes every live forward and releases the API client.
func (t *Tunnel) Close() error {
	t.mu.Lock()
	t.closed = true
	live := t.live
	t.live = map[*fwdConn]struct{}{}
	t.mu.Unlock()
	for c := range live {
		_ = c.Conn.Close()
	}
	return t.k.Close()
}

// Resolve maps "[ns/]kind/name:port" to a concrete pod and container port.
func (t *Tunnel) Resolve(ctx context.Context, addr string) (ns, pod string, port int, err error) {
	ref, portS, ok := cutLast(addr, ":")
	if !ok || !strings.Contains(ref, "/") {
		return "", "", 0, errs.New(errs.Unsupported, "a k8s hop dials kind/name:port targets (got %q)", addr).
			WithHint("use a portforward system: {type: portforward, via: <k8s system>, target: svc/postgres:5432}")
	}
	parts := strings.Split(ref, "/")
	ns = t.k.ns
	if len(parts) == 3 {
		ns, parts = parts[0], parts[1:]
	}
	if len(parts) != 2 {
		return "", "", 0, errs.New(errs.Usage, "bad k8s target %q", addr)
	}
	kind, name := parts[0], parts[1]
	k := t.k
	res, err := k.c.resolve(ctx, kind)
	if err != nil {
		return "", "", 0, err
	}
	if res.Name == "services" {
		return k.resolveService(ctx, ns, name, portS)
	}
	if res.Name == "pods" {
		pod = name
	} else {
		sel, err := k.selectorOf(ctx, ns, res, name)
		if err != nil {
			return "", "", 0, err
		}
		if pod, err = k.readyPod(ctx, ns, sel); err != nil {
			return "", "", 0, err
		}
	}
	port, err = k.podPort(ctx, ns, pod, portS)
	return ns, pod, port, err
}

func cutLast(s, sep string) (string, string, bool) {
	i := strings.LastIndex(s, sep)
	if i < 0 {
		return s, "", false
	}
	return s[:i], s[i+len(sep):], true
}

// resolveService maps a service port (number or name) to a ready backing pod + targetPort.
func (k *K8s) resolveService(ctx context.Context, ns, name, portS string) (string, string, int, error) {
	var svc struct {
		Spec struct {
			Selector map[string]string `json:"selector"`
			Ports    []struct {
				Name       string `json:"name"`
				Port       int    `json:"port"`
				TargetPort any    `json:"targetPort"`
			} `json:"ports"`
		} `json:"spec"`
	}
	if err := k.c.get(ctx, "/api/v1/namespaces/"+url.PathEscape(ns)+"/services/"+url.PathEscape(name), nil, &svc); err != nil {
		return "", "", 0, err
	}
	sb, _ := json.Marshal(svc.Spec.Selector)
	sel, err := selectorString("services", sb)
	if err != nil {
		return "", "", 0, err
	}
	pod, err := k.readyPod(ctx, ns, sel)
	if err != nil {
		return "", "", 0, err
	}
	target := portS
	for _, p := range svc.Spec.Ports {
		if strconv.Itoa(p.Port) == portS || p.Name == portS {
			switch tp := p.TargetPort.(type) {
			case float64:
				target = strconv.Itoa(int(tp))
			case string:
				target = tp
			default:
				target = strconv.Itoa(p.Port)
			}
		}
	}
	port, err := k.podPort(ctx, ns, pod, target)
	return ns, pod, port, err
}

// podPort resolves a numeric or named container port.
func (k *K8s) podPort(ctx context.Context, ns, pod, portS string) (int, error) {
	if n, err := strconv.Atoi(portS); err == nil {
		return n, nil
	}
	var p podObj
	if err := k.c.get(ctx, "/api/v1/namespaces/"+url.PathEscape(ns)+"/pods/"+url.PathEscape(pod), nil, &p); err != nil {
		return 0, err
	}
	for _, c := range p.Spec.Containers {
		for _, cp := range c.Ports {
			if cp.Name == portS {
				return cp.ContainerPort, nil
			}
		}
	}
	return 0, errs.New(errs.NotFound, "pod %s has no port named %q", pod, portS)
}
