package server

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// N servers racing for one socket: exactly one listens, the rest refuse.
func TestConcurrentServersSingleListener(t *testing.T) {
	base := os.TempDir()
	if st, err := os.Stat("/private/tmp/claude-501"); err == nil && st.IsDir() {
		base = "/private/tmp/claude-501"
	}
	dir, err := os.MkdirTemp(base, "tsl")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "s.sock")
	const n = 8
	var wg sync.WaitGroup
	res := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res <- Run(context.Background(), NewEngine(), Options{Socket: sock, Idle: 500 * time.Millisecond})
		}()
	}
	wg.Wait()
	close(res)
	ok, dup := 0, 0
	for err := range res {
		switch err {
		case nil:
			ok++
		case ErrAlreadyRunning:
			dup++
		default:
			t.Fatalf("unexpected: %v", err)
		}
	}
	if ok != 1 || dup != n-1 {
		t.Fatalf("want 1 listener + %d refusals, got %d + %d", n-1, ok, dup)
	}
}

func TestLockPathExclusive(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.lock")
	un, err := LockPath(p, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LockPath(p, false); !IsLocked(err) {
		t.Fatalf("second lock: %v", err)
	}
	un()
	un2, err := LockPath(p, false)
	if err != nil {
		t.Fatal(err)
	}
	un2()
}
