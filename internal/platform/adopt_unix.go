//go:build !windows

package platform

import (
	"syscall"
	"time"
)

// stopByPID stops a process DevMan did not spawn.
//
// The signal is sent to the negated pid, i.e. to the process group, because
// DevMan starts every service as a group leader. That is what makes stopping an
// adopted service still take its children with it.
func stopByPID(pid int, graceful time.Duration) StopOutcome {
	outcome := StopOutcome{}

	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
		// Not a group leader (or the group is already gone): fall back to the
		// process itself rather than giving up on a clean shutdown.
		if directErr := syscall.Kill(pid, syscall.SIGTERM); directErr != nil {
			outcome.SignalError = directErr.Error()
		} else {
			outcome.SignalSent = true
		}
	} else {
		outcome.SignalSent = true
	}

	if outcome.SignalSent && graceful > 0 && waitGone(pid, graceful) {
		outcome.Graceful = true
		return outcome
	}

	_ = syscall.Kill(-pid, syscall.SIGKILL)
	_ = syscall.Kill(pid, syscall.SIGKILL)
	waitGone(pid, 5*time.Second)
	return outcome
}
