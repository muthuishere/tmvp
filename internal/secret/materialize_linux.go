//go:build linux

package secret

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func materializeMem(name string, data []byte) (string, func(), bool) {
	fd, err := unix.MemfdCreate("tmvp-"+safeName(name), 0)
	if err != nil {
		return "", nil, false
	}
	f := os.NewFile(uintptr(fd), "memfd")
	if _, err := f.Write(data); err != nil {
		f.Close()
		return "", nil, false
	}
	// /proc/<pid>/fd/N so child processes (which have their own /proc/self) can open it.
	return fmt.Sprintf("/proc/%d/fd/%d", os.Getpid(), fd), func() { _ = f.Close() }, true
}
