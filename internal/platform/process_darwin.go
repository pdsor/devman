//go:build darwin

package platform

import (
	"bytes"
	"time"

	"golang.org/x/sys/unix"
)

// procExePath resolves the executable of a running process.
//
// macOS has no /proc, and the previous implementation returned nothing at all,
// which quietly made adoption weaker here than on Linux and Windows: identity
// matching fell back to PID plus start time, so a recycled PID belonging to a
// different program with a close enough start time would have been accepted as
// the service DevMan started. The kernel will hand over a process's argument
// area, and its first NUL-terminated string is the resolved executable path, so
// the check can be the same on all three platforms.
//
// The argument area is readable for our own descendants, which is the only case
// adoption cares about; for anything else this returns "" and the caller falls
// back to the weaker comparison rather than refusing.
func procExecutable(pid int) string {
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil || len(raw) < 5 {
		return ""
	}
	// Layout: int32 argc, then the executable path, then argv and the
	// environment, all NUL-terminated.
	if end := bytes.IndexByte(raw[4:], 0); end > 0 {
		return string(raw[4 : 4+end])
	}
	return ""
}

// processName reads the accounting name from the kernel process table.
func processName(pid int) string {
	proc, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return ""
	}
	name := proc.Proc.P_comm
	out := make([]byte, 0, len(name))
	for _, b := range name {
		if b == 0 {
			break
		}
		out = append(out, byte(b))
	}
	return string(out)
}

// processStartTime reads the process creation time from the kernel.
func processStartTime(pid int) (time.Time, error) {
	proc, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return time.Time{}, err
	}
	started := proc.Proc.P_starttime
	return time.Unix(started.Sec, int64(started.Usec)*1000), nil
}
