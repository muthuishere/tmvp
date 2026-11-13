package secure

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestOwnerOnly(t *testing.T) {
	d, err := MkdirTemp(t.TempDir(), "s-")
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(d, "f")
	if err := WriteFile(p, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "x" {
		t.Fatalf("content %q", b)
	}
	if runtime.GOOS == "windows" {
		return
	}
	if st, _ := os.Stat(d); st.Mode().Perm() != 0o700 {
		t.Errorf("dir %v", st.Mode().Perm())
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Errorf("file %v", st.Mode().Perm())
	}
}
