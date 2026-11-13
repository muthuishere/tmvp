package ssh

import (
	"errors"
	"net"
	"os"
	"testing"
	"time"
)

// noDeadline mimics ssh's tcpChan: SetDeadline is unsupported.
type noDeadline struct{ net.Conn }

func (noDeadline) SetDeadline(time.Time) error      { return errors.New("ssh: tcpChan: deadline not supported") }
func (noDeadline) SetReadDeadline(time.Time) error  { return errors.New("ssh: tcpChan: deadline not supported") }
func (noDeadline) SetWriteDeadline(time.Time) error { return errors.New("ssh: tcpChan: deadline not supported") }

func TestDeadlineConn(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	c := withDeadlines(noDeadline{a})
	// a deadline that is reset before it fires changes nothing
	if err := c.SetDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	go b.Write([]byte("hi"))
	buf := make([]byte, 2)
	if _, err := c.Read(buf); err != nil || string(buf) != "hi" {
		t.Fatalf("read %q %v", buf, err)
	}
	c.SetDeadline(time.Time{})
	time.Sleep(80 * time.Millisecond)
	go b.Write([]byte("ok"))
	if _, err := c.Read(buf); err != nil {
		t.Fatalf("cleared deadline still fired: %v", err)
	}
	// a blocked read returns a timeout when the deadline passes
	c.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
	start := time.Now()
	_, err := c.Read(buf)
	var ne net.Error
	if !errors.Is(err, os.ErrDeadlineExceeded) || !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("want timeout, got %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("read did not return promptly")
	}
	// a past deadline fails at once
	d := withDeadlines(noDeadline{b})
	d.SetWriteDeadline(time.Now().Add(-time.Second))
	if _, err := d.Write([]byte("x")); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("past deadline write: %v", err)
	}
}
