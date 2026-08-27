//go:build !windows

package client

import (
	"os/exec"
	"syscall"
)

// detachProcess puts the daemon in its own session, so closing the terminal
// that started it does not send it a hangup.
func detachProcess(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
}
