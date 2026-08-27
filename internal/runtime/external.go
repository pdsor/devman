package runtime

import (
	"sync"
	"time"

	"github.com/devman-project/devman/internal/platform"
	"github.com/devman-project/devman/pkg/config"
)

// ExternalRuntime represents a service DevMan did not start: a database running
// as a system service, a container someone else brought up, a process started
// by hand in another terminal.
//
// It exists so such a service can still appear in `devman status` and be health
// checked and depended upon. DevMan deliberately has no way to start or stop
// it: terminating a process it does not own would be exactly the kind of
// surprise a process manager must never produce.
type ExternalRuntime struct{}

// Kind implements Runtime.
func (ExternalRuntime) Kind() config.RuntimeKind { return config.RuntimeExternal }

// Start does not start anything. It returns a handle that is "running" as far
// as DevMan is concerned; the real answer comes from the health check.
func (ExternalRuntime) Start(req StartRequest) (Handle, error) {
	return &externalHandle{
		line: "external: " + req.Service,
		done: make(chan struct{}),
	}, nil
}

type externalHandle struct {
	line string

	mu     sync.Mutex
	closed bool
	done   chan struct{}
}

// PID is 0: there is no process DevMan owns.
func (h *externalHandle) PID() int                    { return 0 }
func (h *externalHandle) Identity() platform.Identity { return platform.Identity{} }
func (h *externalHandle) CommandLine() string         { return h.line }

func (h *externalHandle) Running() bool {
	select {
	case <-h.done:
		return false
	default:
		return true
	}
}

func (h *externalHandle) Done() <-chan struct{} { return h.done }

func (h *externalHandle) Exit() (platform.ExitStatus, error) {
	if h.Running() {
		return platform.ExitStatus{}, platform.ErrStillRunning
	}
	return platform.ExitStatus{ExitedAt: time.Now().UTC()}, nil
}

// Stop only detaches DevMan's view of the service. No signal is sent.
func (h *externalHandle) Stop(time.Duration) platform.StopOutcome {
	h.mu.Lock()
	if !h.closed {
		h.closed = true
		close(h.done)
	}
	h.mu.Unlock()
	return platform.StopOutcome{
		Graceful: true,
		Status:   platform.ExitStatus{ExitedAt: time.Now().UTC()},
	}
}
