package ssh

import (
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// deadlineConn adds deadlines to an ssh-forwarded connection, whose
// SetDeadline fails with "ssh: tcpChan: deadline not supported" (Redis and
// other drivers set one per operation). When a deadline passes, the
// connection is closed, so a blocked Read/Write returns at once with
// os.ErrDeadlineExceeded (a net.Error whose Timeout() is true). The
// connection is unusable afterwards, which is what drivers expect after a
// timeout: they discard it.
type deadlineConn struct {
	net.Conn
	mu         sync.Mutex
	rt, wt     *time.Timer
	rExp, wExp atomic.Bool
}

func withDeadlines(c net.Conn) net.Conn { return &deadlineConn{Conn: c} }

func (c *deadlineConn) set(tm **time.Timer, exp *atomic.Bool, t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if *tm != nil {
		(*tm).Stop()
		*tm = nil
	}
	if exp.Load() || t.IsZero() {
		return // an expired deadline already closed the conn
	}
	fire := func() { exp.Store(true); _ = c.Conn.Close() }
	if d := time.Until(t); d <= 0 {
		fire()
	} else {
		*tm = time.AfterFunc(d, fire)
	}
}

func (c *deadlineConn) SetReadDeadline(t time.Time) error  { c.set(&c.rt, &c.rExp, t); return nil }
func (c *deadlineConn) SetWriteDeadline(t time.Time) error { c.set(&c.wt, &c.wExp, t); return nil }
func (c *deadlineConn) SetDeadline(t time.Time) error {
	c.set(&c.rt, &c.rExp, t)
	c.set(&c.wt, &c.wExp, t)
	return nil
}

func (c *deadlineConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if err != nil && c.rExp.Load() {
		err = os.ErrDeadlineExceeded
	}
	return n, err
}

func (c *deadlineConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	if err != nil && c.wExp.Load() {
		err = os.ErrDeadlineExceeded
	}
	return n, err
}

func (c *deadlineConn) Close() error {
	c.mu.Lock()
	for _, t := range []*time.Timer{c.rt, c.wt} {
		if t != nil {
			t.Stop()
		}
	}
	c.mu.Unlock()
	return c.Conn.Close()
}
