//go:build windows

package server

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func lockFile(f *os.File, block bool) error {
	flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK)
	if !block {
		flags |= windows.LOCKFILE_FAIL_IMMEDIATELY
	}
	ol := new(windows.Overlapped)
	err := windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, 1, 0, ol)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
		return errLocked
	}
	return err
}
