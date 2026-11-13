//go:build windows

package secret

import (
	"os/exec"
	"syscall"
)

const createNoWindow = 0x08000000

func setProcAttrs(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow}
}
