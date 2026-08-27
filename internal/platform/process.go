// Package platform contains the OS-specific half of the process supervisor:
// spawning a service inside a kill-safe container, signalling it gracefully,
// force-killing its entire process tree, and identifying a process well enough
// to survive a daemon restart.
//
// Reliable start and stop is the highest priority requirement of DevMan V0.1,
// so the platform layer never falls back to "kill the parent PID and hope".
// Windows uses a Job Object plus a dedicated process group; Unix uses a process
// group.
package platform

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ErrStillRunning is returned when an exit status is requested for a process
// that has not exited.
var ErrStillRunning = errors.New("process is still running")

// SpawnRequest describes one process launch. Command must already be resolved
// (PATH lookup, shell wrapping and template expansion happen above this layer).
type SpawnRequest struct {
	Command string
	Args    []string
	Dir     string
	// Env is the complete environment; it is never merged with os.Environ.
	Env []string
	// Stdout and Stderr receive the raw output streams. They are written to
	// from dedicated goroutines and are closed by Process.Close.
	Stdout io.Writer
	Stderr io.Writer
}

// Identity is everything needed to later decide whether a PID is still the
// process DevMan started. A PID alone is never trusted: PIDs are reused.
type Identity struct {
	PID         int       `json:"pid"`
	SpawnedAt   time.Time `json:"spawned_at"`
	Executable  string    `json:"executable"`
	Fingerprint string    `json:"fingerprint"`
}

// CommandFingerprint hashes the command line and working directory so a
// recycled PID running something else can be detected.
func CommandFingerprint(command string, args []string, dir string) string {
	h := sha256.New()
	h.Write([]byte(command))
	h.Write([]byte{0})
	for _, arg := range args {
		h.Write([]byte(arg))
		h.Write([]byte{0})
	}
	h.Write([]byte(dir))
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// ExitStatus is how a process finished.
type ExitStatus struct {
	Code     int       `json:"exit_code"`
	Signal   string    `json:"signal,omitempty"`
	ExitedAt time.Time `json:"exited_at"`
	// Killed reports that DevMan force-terminated the tree.
	Killed bool `json:"killed"`
}

// ProcessInfo is what can be learned about an arbitrary live PID. It is the
// input to reconciliation after a daemon restart, where a stored PID must be
// proven to still be the process DevMan started.
type ProcessInfo struct {
	PID        int       `json:"pid"`
	Name       string    `json:"name,omitempty"`
	Executable string    `json:"executable,omitempty"`
	StartedAt  time.Time `json:"started_at,omitempty"`
}

// MatchesIdentity reports whether a live process is plausibly the one recorded
// in id. Start time is the decisive signal because PIDs are recycled.
func (info ProcessInfo) MatchesIdentity(id Identity) bool {
	if info.PID != id.PID {
		return false
	}
	if !info.StartedAt.IsZero() && !id.SpawnedAt.IsZero() {
		// The recorded spawn time is taken just after CreateProcess/fork, so a
		// small skew is expected. Anything beyond a minute is a different
		// process that inherited the PID.
		skew := info.StartedAt.Sub(id.SpawnedAt)
		if skew < -time.Minute || skew > time.Minute {
			return false
		}
	}
	if info.Executable != "" && id.Executable != "" &&
		!strings.EqualFold(filepath.Base(info.Executable), filepath.Base(id.Executable)) {
		return false
	}
	return true
}

// StopOutcome records how a stop was achieved, which the supervisor surfaces so
// users can see when a service refused to shut down gracefully.
type StopOutcome struct {
	Status ExitStatus `json:"status"`
	// Graceful is true when the process exited after the graceful signal and
	// before the timeout.
	Graceful bool `json:"graceful"`
	// SignalSent reports whether the graceful signal could be delivered at all.
	// On Windows this is false when no console is available to send CTRL_BREAK.
	SignalSent bool `json:"signal_sent"`
	// SignalError explains a failed graceful attempt.
	SignalError string `json:"signal_error,omitempty"`
}

// Process is a running service process and its whole descendant tree.
type Process struct {
	cmd *exec.Cmd
	sys *sysProcess
	id  Identity

	mu     sync.Mutex
	exit   *ExitStatus
	killed bool

	done  chan struct{}
	drain sync.WaitGroup
	// pipeWriters holds the parent's copies of the child's stdout/stderr write
	// ends. They must be closed right after the child starts.
	pipeWriters []io.Closer
}

// Spawn launches the process. The child is placed in its own process group
// (and, on Windows, a Job Object) before it is allowed to run, so no
// descendant can escape supervision.
func Spawn(req SpawnRequest) (*Process, error) {
	if strings.TrimSpace(req.Command) == "" {
		return nil, errors.New("command is required")
	}
	cmd := exec.Command(req.Command, req.Args...)
	cmd.Dir = req.Dir
	cmd.Env = req.Env

	p := &Process{cmd: cmd, done: make(chan struct{})}

	// Output is wired through explicit OS pipes rather than exec's own
	// copying goroutines. That way waiting for process exit never blocks on a
	// grandchild that inherited the pipe and outlived its parent.
	stdoutReader, err := p.attachPipe(&cmd.Stdout)
	if err != nil {
		return nil, err
	}
	stderrReader, err := p.attachPipe(&cmd.Stderr)
	if err != nil {
		stdoutReader.Close()
		return nil, err
	}
	cmd.Stdin = nil

	sys, err := startPlatform(cmd)
	if err != nil {
		p.closeWriters()
		stdoutReader.Close()
		stderrReader.Close()
		return nil, err
	}
	p.sys = sys
	// The parent's copy of each write end must be closed, otherwise the reader
	// never sees EOF after the child exits.
	p.closeWriters()

	exe := req.Command
	if resolved, lookErr := exec.LookPath(req.Command); lookErr == nil {
		exe = resolved
	}
	p.id = Identity{
		PID:         cmd.Process.Pid,
		SpawnedAt:   time.Now().UTC(),
		Executable:  exe,
		Fingerprint: CommandFingerprint(req.Command, req.Args, req.Dir),
	}

	p.pump(stdoutReader, req.Stdout)
	p.pump(stderrReader, req.Stderr)
	go p.monitor()
	return p, nil
}

// attachPipe wires one output stream through an os.Pipe, returning the read end.
func (p *Process) attachPipe(target *io.Writer) (*os.File, error) {
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	*target = writer
	p.pipeWriters = append(p.pipeWriters, writer)
	return reader, nil
}

// closeWriters closes the parent's copies of the pipe write ends.
func (p *Process) closeWriters() {
	for _, closer := range p.pipeWriters {
		_ = closer.Close()
	}
	p.pipeWriters = nil
}

func (p *Process) pump(reader *os.File, sink io.Writer) {
	p.drain.Add(1)
	go func() {
		defer p.drain.Done()
		defer reader.Close()
		if sink == nil {
			_, _ = io.Copy(io.Discard, reader)
			return
		}
		_, _ = io.Copy(sink, reader)
	}()
}

// monitor reaps the process and records its exit status.
func (p *Process) monitor() {
	state, err := p.cmd.Process.Wait()
	status := ExitStatus{ExitedAt: time.Now().UTC()}
	switch {
	case err != nil:
		status.Code = -1
		status.Signal = err.Error()
	default:
		status.Code = state.ExitCode()
		status.Signal = signalName(state)
	}

	p.mu.Lock()
	status.Killed = p.killed
	p.exit = &status
	p.mu.Unlock()

	// Give the output pipes a moment to drain, then stop waiting: a detached
	// grandchild must never be able to keep a stopped service "busy".
	drained := make(chan struct{})
	go func() {
		p.drain.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(2 * time.Second):
	}

	p.sys.release()
	close(p.done)
}

// PID is the direct child's process id, which is also its process group id.
func (p *Process) PID() int { return p.id.PID }

// Identity returns the recorded process identity.
func (p *Process) Identity() Identity { return p.id }

// Done is closed once the process has exited and its output has been drained.
func (p *Process) Done() <-chan struct{} { return p.done }

// Exit returns the exit status, or ErrStillRunning.
func (p *Process) Exit() (ExitStatus, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.exit == nil {
		return ExitStatus{}, ErrStillRunning
	}
	return *p.exit, nil
}

// Running reports whether the process is still being supervised.
func (p *Process) Running() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

// Wait blocks until the process exits.
func (p *Process) Wait() ExitStatus {
	<-p.done
	status, _ := p.Exit()
	return status
}

// SignalGraceful asks the whole process group to shut down cleanly:
// SIGTERM on Unix, CTRL_BREAK on Windows.
func (p *Process) SignalGraceful() error {
	if !p.Running() {
		return nil
	}
	return p.sys.signalGraceful(p.id.PID)
}

// KillTree force-terminates the process and every descendant.
func (p *Process) KillTree() error {
	if !p.Running() {
		return nil
	}
	p.mu.Lock()
	p.killed = true
	p.mu.Unlock()
	return p.sys.killTree(p.id.PID)
}

// Stop performs the full shutdown sequence: graceful signal, bounded wait,
// then a force kill of the entire tree.
func (p *Process) Stop(graceful time.Duration) StopOutcome {
	outcome := StopOutcome{}
	if !p.Running() {
		outcome.Status, _ = p.Exit()
		outcome.Graceful = true
		return outcome
	}

	if err := p.SignalGraceful(); err != nil {
		outcome.SignalError = err.Error()
	} else {
		outcome.SignalSent = true
	}

	if outcome.SignalSent && graceful > 0 {
		select {
		case <-p.done:
			outcome.Graceful = true
			outcome.Status, _ = p.Exit()
			return outcome
		case <-time.After(graceful):
		}
	}

	_ = p.KillTree()
	select {
	case <-p.done:
	case <-time.After(10 * time.Second):
	}
	outcome.Status, _ = p.Exit()
	return outcome
}
