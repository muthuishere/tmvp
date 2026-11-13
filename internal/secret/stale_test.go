package secret

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCleanStale(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	old, cleanup, err := materializeFile("k", []byte("FAKE_SECRET_123"))
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	fresh, c2, err := materializeFile("k", []byte("FAKE_SECRET_123"))
	if err != nil {
		t.Fatal(err)
	}
	defer c2()
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Dir(old), past, past); err != nil {
		t.Fatal(err)
	}
	if n := CleanStale(15 * time.Minute); n != 1 {
		t.Fatalf("removed %d", n)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("stale dir kept")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("fresh dir removed")
	}
}
