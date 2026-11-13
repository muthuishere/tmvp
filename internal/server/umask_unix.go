//go:build !windows

package server

import "syscall"

// withPrivateUmask runs f with umask 0077 so files/sockets it creates are
// owner-only from the first instant. umask is process-wide: only used at
// server start, before any request runs.
func withPrivateUmask(f func()) {
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	f()
}
