package ssh

import (
	"context"
	"net"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

// Tunneler opens an ssh hop for `via:` (ProxyJump semantics).
type Tunneler struct{}

// OpenTunnel connects to the hop through parent (so chains work).
func (Tunneler) OpenTunnel(ctx context.Context, hop *connector.Target, parent connector.Dialer) (connector.Tunnel, error) {
	c, err := Dial(ctx, hop, parent)
	if err != nil {
		return nil, err
	}
	t := &Tunnel{c: c, done: make(chan struct{})}
	go t.keepalive(30 * time.Second)
	return t, nil
}

// Tunnel dials addresses from the ssh host.
type Tunnel struct {
	c    *ssh.Client
	done chan struct{}
}

// DialContext dials addr from the ssh host ("direct-tcpip").
func (t *Tunnel) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	switch network {
	case "tcp", "tcp4", "tcp6", "":
	default:
		return nil, errs.New(errs.Unsupported, "ssh tunnel cannot dial network %q", network)
	}
	c, err := t.c.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, errs.Wrap(errs.Unreachable, err, "via ssh: dial %s", addr)
	}
	return withDeadlines(c), nil
}

// Close closes the ssh client and every connection through it.
func (t *Tunnel) Close() error {
	select {
	case <-t.done:
	default:
		close(t.done)
	}
	return t.c.Close()
}

func (t *Tunnel) keepalive(every time.Duration) {
	tk := time.NewTicker(every)
	defer tk.Stop()
	for {
		select {
		case <-t.done:
			return
		case <-tk.C:
			if _, _, err := t.c.SendRequest("keepalive@openssh.com", true, nil); err != nil {
				return
			}
		}
	}
}
