//go:build !windows

package secret

import (
	"os/exec"
	"syscall"
)

// setProcAttrs puts the child in its own process group so a timeout kills
// the whole tree, and detaches it from any controlling terminal.
func setProcAttrs(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	c.Cancel = func() error {
		if c.Process != nil {
			_ = syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
}
