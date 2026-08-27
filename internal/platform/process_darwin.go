//go:build darwin

package platform

import (
	"time"

	"golang.org/x/sys/unix"
)

// procExePath is empty on macOS: there is no /proc, so the executable path is
// not resolved. Identity matching falls back to PID plus start time, which is
// sufficient to detect PID reuse.
func procExePath(int) string { return "" }

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
