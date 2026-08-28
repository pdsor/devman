//go:build linux || darwin

package platform

import (
	"os"
	"os/exec"
	"syscall"
)

// sysProcess tracks the child's process group. Every descendant inherits the
// group, so signalling the negated pgid reaches the whole tree.
type sysProcess struct {
	pgid int
}

// startPlatform puts the child in a new process group before exec, so no
// descendant can be missed by a later group signal.
func startPlatform(cmd *exec.Cmd) (*sysProcess, error) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.SysProcAttr.Pgid = 0
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &sysProcess{pgid: cmd.Process.Pid}, nil
}

// signalGraceful sends SIGTERM to the whole process group.
func (s *sysProcess) signalGraceful(pid int) error {
	pgid := s.pgid
	if pgid == 0 {
		pgid = pid
	}
	if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil {
		if err == syscall.ESRCH {
			return nil
		}
		// Fall back to the direct child when the group is gone.
		return syscall.Kill(pid, syscall.SIGTERM)
	}
	return nil
}

// killTree sends SIGKILL to the whole process group.
func (s *sysProcess) killTree(pid int) error {
	pgid := s.pgid
	if pgid == 0 {
		pgid = pid
	}
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil {
		if err == syscall.ESRCH {
			return nil
		}
		return syscall.Kill(pid, syscall.SIGKILL)
	}
	return nil
}

// release has nothing to free on Unix.
func (s *sysProcess) release() {}

// signalName reports the terminating signal, if any.
func signalName(state *os.ProcessState) string {
	status, ok := state.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() {
		return ""
	}
	return status.Signal().String()
}

// EnsureConsole is a Windows-only concern.
func EnsureConsole() error { return nil }

// HasConsole is always true on Unix: SIGTERM needs no console.
func HasConsole() bool { return true }

// Alive reports whether a PID currently exists.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	// EPERM means the process exists but belongs to another user.
	return err == syscall.EPERM
}

// Inspect reads the identity of a live PID for reconciliation. Start time is
// best effort and platform specific; a missing value simply weakens the match.
func Inspect(pid int) (ProcessInfo, error) {
	if !Alive(pid) {
		return ProcessInfo{}, syscall.ESRCH
	}
	info := ProcessInfo{PID: pid}
	if started, err := processStartTime(pid); err == nil {
		info.StartedAt = started.UTC()
	}
	// The platform file returns the resolved path, not a link to read: macOS has
	// no /proc, so treating the answer as a symlink silently produced no
	// executable at all there.
	if exe := procExecutable(pid); exe != "" {
		info.Executable = exe
	}
	if info.Executable == "" {
		info.Name = processName(pid)
	} else {
		info.Name = baseName(info.Executable)
	}
	return info, nil
}

func baseName(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[i+1:]
		}
	}
	return path
}
