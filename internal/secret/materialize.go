package secret

import (
	"github.com/muthuishere/tmvp/internal/secure"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const filePrefix = "tmvp-sec-"

// tempBase is where file secrets go: /dev/shm (RAM) on Linux when usable —
// only reached when memfd is unavailable — else the OS temp dir.
func tempBase() string {
	if runtime.GOOS == "linux" {
		if fi, err := os.Stat("/dev/shm"); err == nil && fi.IsDir() {
			if f, err := os.CreateTemp("/dev/shm", ".tmvp-probe-"); err == nil {
				f.Close()
				os.Remove(f.Name())
				return "/dev/shm"
			}
		}
	}
	return os.TempDir()
}

// CleanStale removes tmvp-sec-* dirs older than maxAge left behind by a
// crashed or killed process (cleanup never ran). Called on server start.
// Returns how many it removed.
func CleanStale(maxAge time.Duration) int {
	n := 0
	for _, base := range []string{tempBase(), os.TempDir()} {
		ents, _ := os.ReadDir(base)
		for _, e := range ents {
			if !e.IsDir() || !strings.HasPrefix(e.Name(), filePrefix) {
				continue
			}
			fi, err := e.Info()
			if err != nil || time.Since(fi.ModTime()) < maxAge {
				continue
			}
			if os.RemoveAll(filepath.Join(base, e.Name())) == nil {
				n++
			}
		}
		if base == os.TempDir() {
			break
		}
	}
	return n
}

// Materialize exposes secret bytes as a file path for an external CLI that
// insists on a path (kubectl --kubeconfig, ssh -i, gcloud credentials).
// Linux: memfd (/proc/<pid>/fd/N — never on disk). Elsewhere: a 0600 file in a
// fresh 0700 temp dir. Always call cleanup when the call ends; it closes the
// memfd / deletes the file and dir.
func Materialize(name string, data []byte) (path string, cleanup func(), err error) {
	if p, c, ok := materializeMem(name, data); ok {
		return p, c, nil
	}
	return materializeFile(name, data)
}

func materializeFile(name string, data []byte) (string, func(), error) {
	dir, err := secure.MkdirTemp(tempBase(), filePrefix) // 0700 / owner-only DACL
	if err != nil {
		return "", nil, err
	}
	p := filepath.Join(dir, safeName(name))
	if err := secure.WriteFile(p, data); err != nil {
		os.RemoveAll(dir)
		return "", nil, err
	}
	return p, func() { _ = os.RemoveAll(dir) }, nil
}

func safeName(n string) string {
	out := []byte(n)
	for i, c := range out {
		if !(c == '-' || c == '_' || c == '.' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
			out[i] = '_'
		}
	}
	if len(out) == 0 {
		return "secret"
	}
	return string(out)
}
