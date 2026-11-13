package server

import (
	"errors"
	"os"

	"github.com/muthuishere/tmvp/internal/secure"
)

var errLocked = errors.New("lock held by another process")

// LockPath takes an exclusive OS lock (flock / LockFileEx) on path,
// creating it 0600. With block=false it returns ErrAlreadyRunning-style
// errLocked immediately when another process holds it. The lock is released
// by the returned func or when the process dies (never left stale).
func LockPath(path string, block bool) (unlock func(), err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockFile(f, block); err != nil {
		f.Close()
		return nil, err
	}
	return func() { f.Close() }, nil
}

// IsLocked reports whether err means the lock is held elsewhere.
func IsLocked(err error) bool { return errors.Is(err, errLocked) }

// SecureRunDir makes dir owner-only (0700 on Unix; protected owner+SYSTEM
// DACL on Windows).
func SecureRunDir(dir string) error {
	return secure.MkdirAll(dir)
}
