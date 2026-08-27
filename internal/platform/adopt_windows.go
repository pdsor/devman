//go:build windows

package platform

import (
	"time"

	"golang.org/x/sys/windows"
)

// stopByPID stops a process DevMan did not spawn.
//
// Adoption is a weaker guarantee on Windows: the Job Object that contained the
// original process tree belonged to the previous daemon and was destroyed with
// it, so only the process itself and any group members still attached to the
// console can be reached. In practice a Windows service tree does not survive
// its daemon at all, because the job is created with
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE; this path exists for the rare survivor.
func stopByPID(pid int, graceful time.Duration) StopOutcome {
	outcome := StopOutcome{}

	if hasConsole() {
		if err := windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(pid)); err != nil {
			outcome.SignalError = err.Error()
		} else {
			outcome.SignalSent = true
		}
	} else {
		outcome.SignalError = "no console attached: CTRL_BREAK cannot be delivered"
	}

	if outcome.SignalSent && graceful > 0 && waitGone(pid, graceful) {
		outcome.Graceful = true
		return outcome
	}

	handle, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return outcome
	}
	defer windows.CloseHandle(handle)
	_ = windows.TerminateProcess(handle, 1)
	waitGone(pid, 5*time.Second)
	return outcome
}
