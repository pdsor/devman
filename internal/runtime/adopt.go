package runtime

import (
	"time"

	"github.com/devman-project/devman/internal/platform"
)

// Adopted wraps a process a previous daemon started so the supervisor can treat
// it like any other instance.
//
// The difference is visible where it matters and nowhere else: the handle
// reports the same interface, while the service's observability says
// `log_capture: detached`, because the pipes belonged to the daemon that is
// gone. Restarting the service is what restores full capture.
func Adopted(id platform.Identity, commandLine string) Handle {
	return &adoptedHandle{
		proc: platform.Adopt(id),
		line: commandLine,
	}
}

type adoptedHandle struct {
	proc *platform.AdoptedProcess
	line string
}

func (h *adoptedHandle) PID() int                    { return h.proc.PID() }
func (h *adoptedHandle) Identity() platform.Identity { return h.proc.Identity() }
func (h *adoptedHandle) CommandLine() string         { return h.line }
func (h *adoptedHandle) Running() bool               { return h.proc.Running() }
func (h *adoptedHandle) Done() <-chan struct{}       { return h.proc.Done() }

func (h *adoptedHandle) Exit() (platform.ExitStatus, error) { return h.proc.Exit() }

func (h *adoptedHandle) Stop(graceful time.Duration) platform.StopOutcome {
	return h.proc.Stop(graceful)
}
