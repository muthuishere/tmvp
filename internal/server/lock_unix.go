//go:build !windows

package server

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func lockFile(f *os.File, block bool) error {
	how := unix.LOCK_EX
	if !block {
		how |= unix.LOCK_NB
	}
	for {
		err := unix.Flock(int(f.Fd()), how)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if errors.Is(err, unix.EWOULDBLOCK) {
			return errLocked
		}
		return err
	}
}
