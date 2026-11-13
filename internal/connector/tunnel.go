package connector

import (
	"context"
	"fmt"
	"net"
	"sort"
	"sync"
	"time"
)

// Dialer opens network connections. Target.Dialer implements `via:`.
type Dialer interface {
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
}

// DialerFunc adapts a func to Dialer.
type DialerFunc func(ctx context.Context, network, addr string) (net.Conn, error)

func (f DialerFunc) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return f(ctx, network, addr)
}

// DirectDialer dials without any tunnel.
func DirectDialer() Dialer {
	return &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
}

// Tunnel is an open hop. Its DialContext reaches the network "behind" the hop:
//   - ssh hop: dials addr from the ssh host (like ProxyJump);
//   - portforward hop: ignores addr and connects to the hop's fixed `remote:`.
type Tunnel interface {
	Dialer
	Close() error
}

// Tunneler builds a Tunnel for one hop system (type ssh, portforward, k8s).
//
// The server resolves a system's `via:` chain in order: for
// `via: [bastion, jump2]` it opens bastion with parent=DirectDialer, then
// jump2 with parent=bastion's tunnel, and gives the target jump2's tunnel as
// its Dialer. A hop's own `via:` is resolved first. Tunnels are shared per
// product/env/hop and closed on idle/reload. hop.Mode is always ModeRead.
type Tunneler interface {
	OpenTunnel(ctx context.Context, hop *Target, parent Dialer) (Tunnel, error)
}

var (
	tunMu     sync.RWMutex
	tunnelers = map[string]Tunneler{}
)

// RegisterTunneler registers the tunnel provider for a hop type. Call from init().
func RegisterTunneler(typ string, t Tunneler) {
	tunMu.Lock()
	defer tunMu.Unlock()
	if _, dup := tunnelers[typ]; dup {
		panic(fmt.Sprintf("connector: tunneler %q registered twice", typ))
	}
	tunnelers[typ] = t
}

// LookupTunneler returns the tunneler for a hop type.
func LookupTunneler(typ string) (Tunneler, bool) {
	tunMu.RLock()
	defer tunMu.RUnlock()
	t, ok := tunnelers[typ]
	return t, ok
}

// TunnelerTypes lists registered tunneler types.
func TunnelerTypes() []string {
	tunMu.RLock()
	defer tunMu.RUnlock()
	var out []string
	for t := range tunnelers {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}
