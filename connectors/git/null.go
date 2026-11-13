package git

import (
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/muthuishere/tmvp/internal/protocol"
	"github.com/muthuishere/tmvp/internal/secure"
)

// Git for Windows rejects "NUL" (os.DevNull) as a config file path or hooks
// dir, so on Windows tmvp points git at a real empty file and an empty dir.
var nullOnce struct {
	sync.Once
	cfg, hooks string
}

func nullPaths() (cfg, hooks string) {
	if runtime.GOOS != "windows" {
		return os.DevNull, os.DevNull
	}
	nullOnce.Do(func() {
		dir := filepath.Join(protocol.Home(), "git-null")
		nullOnce.cfg, nullOnce.hooks = os.DevNull, os.DevNull
		if secure.MkdirAll(filepath.Join(dir, "hooks")) != nil {
			return
		}
		cfg := filepath.Join(dir, "config")
		if secure.WriteFile(cfg, nil) != nil {
			return
		}
		nullOnce.cfg, nullOnce.hooks = cfg, filepath.Join(dir, "hooks")
	})
	return nullOnce.cfg, nullOnce.hooks
}

func emptyConfigPath() string { c, _ := nullPaths(); return c }
func noHooksPath() string     { _, h := nullPaths(); return h }
