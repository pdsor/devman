// Package runtime abstracts "how a service is executed" behind one interface,
// so the supervisor above it contains no host-versus-docker branching.
//
// Three runtimes exist in V0.1:
//
//	host            a process DevMan spawns and owns
//	docker-compose  a compose service DevMan brings up and follows
//	external        something started elsewhere, which DevMan only observes
//
// The distinction matters for stopping: DevMan must never terminate a process
// it did not start, so ExternalRuntime has no stop path at all.
package runtime

import (
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/devman-project/devman/internal/platform"
	"github.com/devman-project/devman/pkg/config"
	"github.com/devman-project/devman/pkg/errs"
)

// StartRequest is one launch, fully resolved: templates expanded, ports
// allocated, environment merged, executable located.
type StartRequest struct {
	Project string
	Service string

	// Command is the executable to run, or the shell line when Shell is
	// enabled.
	Command string
	Args    []string
	Shell   config.ShellSpec
	Dir     string
	// Env is the complete environment; it is never merged with the daemon's.
	Env []string

	Stdout io.Writer
	Stderr io.Writer

	// Compose is required by the docker-compose runtime.
	Compose *config.ComposeSpec
	// GracefulTimeout is passed to compose stop, which needs it up front.
	GracefulTimeout time.Duration
}

// Handle is a started service instance.
type Handle interface {
	// PID is the supervised process id, or 0 when the runtime has none.
	PID() int
	Identity() platform.Identity
	// CommandLine is what DevMan actually executed, for display and diagnosis.
	CommandLine() string
	Running() bool
	// Done is closed once the instance has finished.
	Done() <-chan struct{}
	Exit() (platform.ExitStatus, error)
	// Stop shuts the instance down: graceful first, then forced.
	Stop(graceful time.Duration) platform.StopOutcome
}

// Runtime starts services of one kind.
type Runtime interface {
	Kind() config.RuntimeKind
	Start(req StartRequest) (Handle, error)
}

// Set groups the runtimes the supervisor can dispatch to.
type Set struct {
	Host     Runtime
	Compose  Runtime
	External Runtime
}

// NewSet builds the standard runtime set.
func NewSet() Set {
	return Set{
		Host:     HostRuntime{},
		Compose:  ComposeRuntime{},
		External: ExternalRuntime{},
	}
}

// For selects the runtime of a service kind.
func (s Set) For(kind config.RuntimeKind) (Runtime, error) {
	switch kind {
	case config.RuntimeHost, "":
		return s.Host, nil
	case config.RuntimeDockerCompose:
		return s.Compose, nil
	case config.RuntimeExternal:
		return s.External, nil
	default:
		return nil, errs.New(errs.CodeConfigInvalid, "unknown runtime %q", kind)
	}
}

// CommandLine renders a command and its arguments for display. It is not a
// shell-safe quoting routine and is never fed back into a shell.
func CommandLine(command string, args []string) string {
	if len(args) == 0 {
		return command
	}
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, command)
	for _, arg := range args {
		if strings.ContainsAny(arg, " \t\"") {
			parts = append(parts, `"`+arg+`"`)
			continue
		}
		parts = append(parts, arg)
	}
	return strings.Join(parts, " ")
}

// processHandle adapts platform.Process to the Handle interface.
type processHandle struct {
	proc *platform.Process
	line string
}

func (h *processHandle) PID() int                           { return h.proc.PID() }
func (h *processHandle) Identity() platform.Identity        { return h.proc.Identity() }
func (h *processHandle) CommandLine() string                { return h.line }
func (h *processHandle) Running() bool                      { return h.proc.Running() }
func (h *processHandle) Done() <-chan struct{}              { return h.proc.Done() }
func (h *processHandle) Exit() (platform.ExitStatus, error) { return h.proc.Exit() }

func (h *processHandle) Stop(graceful time.Duration) platform.StopOutcome {
	return h.proc.Stop(graceful)
}

// lookPath is a thin wrapper so a missing interpreter produces DevMan's own
// error code rather than an opaque exec error.
func lookPath(name string) (string, error) {
	resolved, err := exec.LookPath(name)
	if err != nil {
		return "", errs.New(errs.CodeCommandNotFound, "%s was not found on PATH", name).
			With("command", name)
	}
	return resolved, nil
}
