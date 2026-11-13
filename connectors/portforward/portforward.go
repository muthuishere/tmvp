// Package portforward is tmvp's portforward system type: a fixed tunnel to
// one remote endpoint, reached through its own via: (ssh or k8s).
//
//	db-tunnel: { type: portforward, via: bastion, remote: db.internal:5432 }
//	pg-fwd:    { type: portforward, via: cluster, target: svc/postgres:5432, namespace: data }
//
// As a hop (`via: db-tunnel`) it ignores the address its user dials and
// connects to its fixed remote. As a system it has read verbs status, open
// (a local 127.0.0.1 listener kept by the warm connector, for plain psql/kubectl) and close.
package portforward

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

func init() {
	connector.Register("portforward", New)
	connector.RegisterTunneler("portforward", Tunneler{})
}

// K8sNetwork is the dial network understood by the k8s tunnel (connectors/k8s.Network).
const K8sNetwork = "k8s"

// New returns an unopened portforward connector.
func New() connector.Connector { return &PF{} }

// PF is one portforward system.
type PF struct {
	t       *connector.Target
	network string
	addr    string

	mu sync.Mutex
	ln net.Listener
	wg sync.WaitGroup
}

var verbs = []connector.Verb{
	{Name: "ls", Mode: connector.Read, Help: "ls — what this hop reaches: remote, via chain, local listener (no dial)"},
	{Name: "status", Mode: connector.Read, Help: "dial the remote through the tunnel; latency, listener"},
	{Name: "open", Mode: connector.Read, Help: "open a local 127.0.0.1 listener on local_port: or the tmvp port convention (= export pg-service) [-F port=N] (kept while the server keeps the system warm)"},
	{Name: "close", Mode: connector.Read, Help: "close the local listener"},
}

func (p *PF) Type() string            { return "portforward" }
func (p *PF) Verbs() []connector.Verb { return verbs }

// Destination resolves the fixed endpoint: remote: host:port, or target: kind/name:port (+ namespace) via k8s.
func Destination(ctx context.Context, t *connector.Target) (network, addr string, err error) {
	if t.Has("remote") {
		r, err := t.String(ctx, "remote")
		if err != nil {
			return "", "", err
		}
		if _, port, err := net.SplitHostPort(r); err != nil || port == "" {
			return "", "", errs.New(errs.Config, "portforward: remote must be host:port")
		}
		return "tcp", r, nil
	}
	if t.Has("target") {
		tg, err := t.String(ctx, "target")
		if err != nil {
			return "", "", err
		}
		i := strings.LastIndex(tg, ":")
		if i < 0 || !strings.Contains(tg[:i], "/") {
			return "", "", errs.New(errs.Config, "portforward: target must be kind/name:port (e.g. svc/postgres:5432)")
		}
		ns, err := t.StringOr(ctx, "namespace", "")
		if err != nil {
			return "", "", err
		}
		if ns != "" && strings.Count(tg[:i], "/") == 1 {
			tg = ns + "/" + tg
		}
		return K8sNetwork, tg, nil
	}
	return "", "", errs.New(errs.Config, "portforward: set remote: host:port (via ssh) or target: svc/name:port (via k8s)")
}

// Open resolves the destination; the tunnel itself is the target's Dialer (its via chain).
func (p *PF) Open(ctx context.Context, t *connector.Target) error {
	p.t = t
	n, a, err := Destination(ctx, t)
	if err != nil {
		return err
	}
	if len(t.Via) == 0 && n == K8sNetwork {
		return errs.New(errs.Config, "portforward: target: needs via: <k8s system>")
	}
	p.network, p.addr = n, a
	return nil
}

func (p *PF) dial(ctx context.Context) (net.Conn, error) {
	c, err := p.t.Dial(ctx, p.network, p.addr)
	if err != nil {
		if e := errs.From(err); e != nil {
			return nil, e
		}
		return nil, errs.Wrap(errs.Unreachable, err, "portforward %s", p.addr)
	}
	return c, nil
}

// Check implements connector.Checker.
func (p *PF) Check(ctx context.Context) error {
	c, err := p.dial(ctx)
	if err != nil {
		return err
	}
	return c.Close()
}

// Do runs one verb.
func (p *PF) Do(ctx context.Context, c connector.Call, e connector.Emit) error {
	switch c.Verb {
	case "ls":
		return ignoreLimit(e.Row(connector.Row{"kind": "forward", "name": p.t.System, "remote": p.addr,
			"via": strings.Join(p.t.Via, ">"), "listener": p.listenAddr(),
			"default_port": connector.LocalPort(p.t.Product, p.t.Env, p.t.System)}))
	case "status":
		start := time.Now()
		r := connector.Row{"remote": p.addr, "via": strings.Join(p.t.Via, ">"), "listener": p.listenAddr()}
		conn, err := p.dial(ctx)
		if err != nil {
			r["ok"], r["error"] = false, string(errs.CodeOf(err))
		} else {
			conn.Close()
			r["ok"], r["latency_ms"] = true, time.Since(start).Milliseconds()
		}
		return ignoreLimit(e.Row(r))
	case "open":
		port := c.Flags["port"]
		var err error
		if port == "" {
			// the convention `tmvp export pg-service` prints (local_port: overrides)
			if port, err = p.t.StringOr(ctx, "local_port", ""); err != nil {
				return err
			}
			if port == "" {
				port = connector.LocalPortString(p.t.Product, p.t.Env, p.t.System)
			}
		}
		addr, err := p.listen(port)
		if err != nil {
			return err
		}
		return ignoreLimit(e.Row(connector.Row{"listener": addr, "remote": p.addr, "ok": true}))
	case "close":
		was := p.listenAddr()
		p.closeListener()
		return ignoreLimit(e.Row(connector.Row{"closed": was != "", "listener": was}))
	}
	return errs.New(errs.Usage, "portforward has no verb %q", c.Verb)
}

func (p *PF) listenAddr() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ln == nil {
		return ""
	}
	return p.ln.Addr().String()
}

// listen starts (or returns) the local listener on 127.0.0.1.
func (p *PF) listen(port string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ln != nil {
		return p.ln.Addr().String(), nil
	}
	if port == "" {
		port = "0"
	}
	if _, err := strconv.Atoi(port); err != nil {
		return "", errs.New(errs.Usage, "port must be a number")
	}
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		return "", errs.Wrap(errs.Runtime, err, "listen")
	}
	p.ln = ln
	p.wg.Add(1)
	go p.serve(ln)
	return ln.Addr().String(), nil
}

func (p *PF) serve(ln net.Listener) {
	defer p.wg.Done()
	for {
		lc, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer lc.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			rc, err := p.dial(ctx)
			cancel()
			if err != nil {
				return
			}
			defer rc.Close()
			pipe(lc, rc)
		}()
	}
}

func pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		} else {
			_ = dst.Close()
		}
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	<-done
}

func (p *PF) closeListener() {
	p.mu.Lock()
	ln := p.ln
	p.ln = nil
	p.mu.Unlock()
	if ln != nil {
		ln.Close()
		p.wg.Wait()
	}
}

// Close stops the listener.
func (p *PF) Close() error { p.closeListener(); return nil }

func ignoreLimit(err error) error {
	if errors.Is(err, connector.ErrLimit) {
		return nil
	}
	return err
}

// ---- tunneler ----

// Tunneler makes a portforward system usable as a via: hop.
type Tunneler struct{}

// OpenTunnel resolves the fixed destination and verifies it is reachable through parent.
func (Tunneler) OpenTunnel(ctx context.Context, hop *connector.Target, parent connector.Dialer) (connector.Tunnel, error) {
	n, a, err := Destination(ctx, hop)
	if err != nil {
		return nil, err
	}
	if parent == nil {
		parent = connector.DirectDialer()
	}
	t := &Tunnel{parent: parent, network: n, addr: a}
	c, err := t.DialContext(ctx, "tcp", "")
	if err != nil {
		return nil, err
	}
	c.Close()
	return t, nil
}

// Tunnel ignores the dialed address and connects to its fixed remote.
type Tunnel struct {
	parent        connector.Dialer
	network, addr string
}

// DialContext connects to the fixed remote through the hop's own via.
func (t *Tunnel) DialContext(ctx context.Context, _, _ string) (net.Conn, error) {
	c, err := t.parent.DialContext(ctx, t.network, t.addr)
	if err != nil {
		if e := errs.From(err); e != nil {
			return nil, e
		}
		return nil, errs.Wrap(errs.Unreachable, err, "portforward %s", t.addr)
	}
	return c, nil
}

// Close is a no-op: the parent hop owns the connection.
func (t *Tunnel) Close() error { return nil }
