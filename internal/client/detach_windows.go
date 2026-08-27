//go:build windows

package client

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// detachProcess starts the daemon fully detached from the CLI.
//
// DETACHED_PROCESS gives it no console, which is why the daemon calls
// AllocConsole at startup: it needs a console of its own (hidden) to deliver
// CTRL_BREAK to service process groups, and inheriting the CLI's console would
// tie its lifetime to the terminal window.
func detachProcess(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP
}
