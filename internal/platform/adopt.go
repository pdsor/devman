package platform

import (
	"sync"
	"time"
)

// adoptPollInterval is how often an adopted process is checked. It is polling
// rather than waiting because the daemon is not the parent: there is no handle
// to wait on and no SIGCHLD to receive.
const adoptPollInterval = 500 * time.Millisecond

// AdoptedProcess is a process a previous daemon started and this one took over.
//
// Adoption is deliberately weaker than supervision: DevMan is not the parent, so
// it cannot read the exit code, and on Windows it cannot reach descendants that
// were contained by the previous daemon's Job Object. What it can do is observe
// liveness, report it honestly, and still stop the process on request.
type AdoptedProcess struct {
	id Identity

	mu   sync.Mutex
	exit *ExitStatus

	done     chan struct{}
	closeAll sync.Once
	stopPoll chan struct{}
}

// Adopt begins observing an existing process. The identity is re-checked on
// every poll, so a recycled PID is treated as the process having exited rather
// than silently supervising a stranger.
func Adopt(id Identity) *AdoptedProcess {
	p := &AdoptedProcess{
		id:       id,
		done:     make(chan struct{}),
		stopPoll: make(chan struct{}),
	}
	go p.poll()
	return p
}

func (p *AdoptedProcess) poll() {
	ticker := time.NewTicker(adoptPollInterval)
	defer ticker.Stop()
	for {
		if !p.stillThere() {
			p.markExited(false)
			return
		}
		select {
		case <-p.stopPoll:
			return
		case <-ticker.C:
		}
	}
}

// stillThere reports whether the adopted PID is still the process DevMan
// recorded.
func (p *AdoptedProcess) stillThere() bool {
	if !Alive(p.id.PID) {
		return false
	}
	info, err := Inspect(p.id.PID)
	if err != nil {
		// The process is alive but cannot be inspected (a permission quirk, or a
		// platform that cannot report start times). Liveness is the better
		// answer than a false negative here.
		return true
	}
	return info.MatchesIdentity(p.id)
}

func (p *AdoptedProcess) markExited(killed bool) {
	p.mu.Lock()
	if p.exit == nil {
		// The exit code of a process DevMan did not spawn is unavailable; -1
		// records "unknown" rather than pretending it exited cleanly.
		p.exit = &ExitStatus{Code: -1, ExitedAt: time.Now().UTC(), Killed: killed}
	}
	p.mu.Unlock()
	p.closeAll.Do(func() {
		close(p.stopPoll)
		close(p.done)
	})
}

// PID is the adopted process id.
func (p *AdoptedProcess) PID() int { return p.id.PID }

// Identity returns the recorded identity.
func (p *AdoptedProcess) Identity() Identity { return p.id }

// Done is closed once the process is observed to be gone.
func (p *AdoptedProcess) Done() <-chan struct{} { return p.done }

// Running reports whether the process is still there.
func (p *AdoptedProcess) Running() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

// Exit returns the observed exit, or ErrStillRunning.
func (p *AdoptedProcess) Exit() (ExitStatus, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.exit == nil {
		return ExitStatus{}, ErrStillRunning
	}
	return *p.exit, nil
}

// Stop terminates the adopted process: a graceful signal first, then a kill.
func (p *AdoptedProcess) Stop(graceful time.Duration) StopOutcome {
	if !p.Running() {
		status, _ := p.Exit()
		return StopOutcome{Status: status, Graceful: true}
	}
	outcome := stopByPID(p.id.PID, graceful)
	p.markExited(!outcome.Graceful)
	status, _ := p.Exit()
	outcome.Status = status
	return outcome
}

// waitGone polls until the process disappears or the deadline passes.
func waitGone(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !Alive(pid) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return !Alive(pid)
}
