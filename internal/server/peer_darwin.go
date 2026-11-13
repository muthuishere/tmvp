//go:build darwin

package server

import (
	"fmt"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// checkPeer refuses connections from other OS users (LOCAL_PEERCRED).
func checkPeer(c net.Conn) error {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return fmt.Errorf("not a unix connection")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return err
	}
	var cred *unix.Xucred
	var cerr error
	if err := raw.Control(func(fd uintptr) { cred, cerr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED) }); err != nil {
		return err
	}
	if cerr != nil {
		return cerr
	}
	if int(cred.Uid) != os.Getuid() {
		return fmt.Errorf("peer uid %d is not %d", cred.Uid, os.Getuid())
	}
	return nil
}
