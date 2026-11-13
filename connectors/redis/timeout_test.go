package redis

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/muthuishere/tmvp/internal/connector"
	"github.com/muthuishere/tmvp/internal/errs"
)

// slowRedis is a minimal RESP2 fake: handshake commands answer at once, every
// SCAN answers after delay with a never-ending cursor, GET never answers.
func slowRedis(t *testing.T, delay time.Duration) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveSlow(conn, delay)
		}
	}()
	return ln.Addr().String()
}

func serveSlow(conn net.Conn, delay time.Duration) {
	defer conn.Close()
	rd := bufio.NewReader(conn)
	n := 0
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			return
		}
		cnt, _ := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "*")))
		var args []string
		for i := 0; i < cnt; i++ {
			if _, err := rd.ReadString('\n'); err != nil { // $len
				return
			}
			a, err := rd.ReadString('\n')
			if err != nil {
				return
			}
			args = append(args, strings.TrimSpace(a))
		}
		if len(args) == 0 {
			return
		}
		switch strings.ToUpper(args[0]) {
		case "HELLO":
			fmt.Fprint(conn, "-ERR unknown command 'HELLO'\r\n")
		case "PING":
			fmt.Fprint(conn, "+PONG\r\n")
		case "SCAN":
			time.Sleep(delay)
			n++
			k := fmt.Sprintf("k%d", n)
			fmt.Fprintf(conn, "*2\r\n$1\r\n1\r\n*1\r\n$%d\r\n%s\r\n", len(k), k)
		case "GET":
			time.Sleep(time.Hour) // until the client hangs up
		default:
			fmt.Fprint(conn, "+OK\r\n")
		}
	}
}

func openSlow(t *testing.T, addr string, lim connector.Limits) *Redis {
	t.Helper()
	r := &Redis{}
	tg := connector.NewTarget(connector.Target{Mode: connector.ModeRead, Limits: lim},
		connector.StaticFields{"read": map[string]any{"url": "redis://" + addr}})
	if err := r.Open(context.Background(), tg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

// The call timeout bounds the whole verb: each SCAN round-trip (80ms) is well
// inside the per-command statement timeout (5s), yet the scan as a whole must
// stop near the 400ms call budget instead of looping on.
func TestCallTimeoutBoundsWholeRead(t *testing.T) {
	addr := slowRedis(t, 80*time.Millisecond)
	r := openSlow(t, addr, connector.Limits{Timeout: 400 * time.Millisecond, StatementTimeout: 5 * time.Second})
	for _, c := range []connector.Call{{Verb: "scan", Flags: map[string]string{}}, {Verb: "get", Args: []string{"k"}}} {
		start := time.Now()
		err := r.Do(context.Background(), c, connector.EmitFuncs{})
		el := time.Since(start)
		if e := errs.From(err); e == nil || e.Code != errs.Timeout {
			t.Fatalf("%s: want timeout, got %v", c.Verb, err)
		}
		if el > 2*time.Second {
			t.Fatalf("%s: took %s, call timeout not applied to the whole verb", c.Verb, el)
		}
	}
}
