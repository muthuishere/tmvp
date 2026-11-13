package client

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/muthuishere/tmvp/internal/protocol"
	"github.com/muthuishere/tmvp/internal/server"
)

// TestMain lets the test binary act as `tmvp server run` when StartServer
// re-executes it (os.Executable is the test binary). Each spawned process
// appends one line to $TMVP_TEST_SPAWNLOG.
func TestMain(m *testing.M) {
	if os.Getenv("TMVP_SERVER_CHILD") == "1" && len(os.Args) >= 3 && os.Args[len(os.Args)-2] == "server" && os.Args[len(os.Args)-1] == "run" {
		logLine := func(s string) {
			if p := os.Getenv("TMVP_TEST_SPAWNLOG"); p != "" {
				f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
				if err == nil {
					fmt.Fprintf(f, "%d %s\n", os.Getpid(), s)
					f.Close()
				}
			}
		}
		logLine("spawn")
		err := server.Run(context.Background(), server.NewEngine(), server.Options{})
		if errors.Is(err, server.ErrAlreadyRunning) {
			logLine("dup")
			os.Exit(0)
		}
		if err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func shortTempDir(t *testing.T) string {
	t.Helper()
	base := os.TempDir()
	if st, err := os.Stat("/private/tmp/claude-501"); err == nil && st.IsDir() {
		base = "/private/tmp/claude-501"
	}
	d, err := os.MkdirTemp(base, "tas")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func TestConcurrentAutostartIsSingleFlight(t *testing.T) {
	home := shortTempDir(t)
	spawnlog := filepath.Join(home, "spawn.log")
	t.Setenv("TMVP_HOME", home)
	t.Setenv("TMVP_SOCKET", "")
	t.Setenv("TMVP_NO_SERVER", "")
	t.Setenv("TMVP_IDLE", "3s")
	t.Setenv("TMVP_TEST_SPAWNLOG", spawnlog)
	sock := protocol.SocketPath()
	if len(sock) > 103 {
		t.Skipf("socket path too long: %s", sock)
	}
	t.Cleanup(func() { // stop the server so it does not outlive the test
		_, _ = Do(context.Background(), protocol.Request{Op: protocol.OpStop}, func(protocol.Frame) {})
	})

	const n = 8
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	errc := make(chan error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			c, err := Dial(ctx, true)
			if err != nil {
				errc <- err
				return
			}
			c.Close()
		}()
	}
	close(start)
	wg.Wait()
	close(errc)
	for err := range errc {
		t.Fatalf("dial: %v", err)
	}
	b, err := os.ReadFile(spawnlog)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	spawns := 0
	for _, l := range lines {
		if strings.HasSuffix(l, " spawn") {
			spawns++
		}
		if strings.HasSuffix(l, " dup") {
			t.Errorf("a redundant server was spawned: %q", l)
		}
	}
	if spawns != 1 {
		t.Fatalf("want exactly 1 server spawn, got %d:\n%s", spawns, b)
	}
}
