package supervisor

import (
	"fmt"
	"io"
	"math/rand"
	"time"

	"github.com/devman-project/devman/internal/health"
	"github.com/devman-project/devman/internal/logstore"
	"github.com/devman-project/devman/internal/platform"
	"github.com/devman-project/devman/internal/runtime"
	"github.com/devman-project/devman/internal/storage"
	"github.com/devman-project/devman/pkg/config"
	"github.com/devman-project/devman/pkg/dto"
	"github.com/devman-project/devman/pkg/errs"
)

// maxBindWait bounds how long DevMan polls for a service to bind its ports.
// It is deliberately shorter than the start timeout: a slow bundler may take a
// minute to be useful, but nobody benefits from probing sockets for a minute.
const maxBindWait = 15 * time.Second

// launch starts one instance. The caller must hold sv.opMu.
func (sv *service) launch(cfg *config.Config, spec *config.Service, isRestart bool) error {
	if sv.running() {
		return errs.New(errs.CodeAlreadyRunning, "service %s is already running", sv.name)
	}

	sv.mu.Lock()
	sv.kind = spec.Runtime
	// Bumping the generation invalidates the watcher of any previous instance,
	// so an exit that has already been accounted for cannot be counted twice.
	sv.generation++
	generation := sv.generation
	if !isRestart {
		sv.restarts = 0
	}
	sv.mu.Unlock()

	// The desired state is recorded before anything is spawned. If the daemon
	// dies mid-start, reconciliation still knows this service was meant to run.
	sv.setDesired(dto.DesiredRunning)
	sv.setStatus(dto.StatusStarting, "", nil)
	sv.emit(dto.EventServiceStarting, "starting "+sv.name, nil)

	plan, err := sv.prepare(cfg, spec)
	if err != nil {
		sv.fail(err)
		return err
	}

	rt, err := sv.sup.deps.Runtimes.For(spec.Runtime)
	if err != nil {
		_ = sv.sup.deps.Ports.ReleaseService(sv.projectID, sv.name)
		sv.fail(err)
		return err
	}

	stdout, stderr := sv.logWriters()
	displayLine := runtime.CommandLine(plan.CommandRaw, plan.Args)
	sv.log("starting: " + displayLine + " (cwd: " + plan.Dir + ")")

	handle, err := rt.Start(runtime.StartRequest{
		Project:         sv.projectID,
		Service:         sv.name,
		Command:         plan.Command,
		Args:            plan.Args,
		Shell:           plan.Shell,
		Dir:             plan.Dir,
		Env:             plan.Env,
		Stdout:          stdout,
		Stderr:          stderr,
		Compose:         plan.Compose,
		GracefulTimeout: plan.Graceful,
	})
	if err != nil {
		_ = sv.sup.deps.Ports.ReleaseService(sv.projectID, sv.name)
		sv.log("failed to start: " + err.Error())
		sv.fail(err)
		return err
	}

	instanceID := newInstanceID()
	now := time.Now().UTC()

	sv.mu.Lock()
	sv.handle = handle
	sv.instanceID = instanceID
	sv.commandLine = displayLine
	sv.cwd = plan.Dir
	sv.startedAt = now
	sv.stoppedAt = time.Time{}
	sv.actual = dto.StatusRunning
	sv.message = ""
	sv.reason = nil
	sv.adopted = false
	sv.healthSpec = plan.Health
	if spec.Runtime == config.RuntimeExternal {
		sv.logCapture = dto.LogCaptureNone
	} else {
		sv.logCapture = dto.LogCaptureAttached
	}
	sv.mu.Unlock()
	sv.persist()

	// The allocations are published immediately so a caller sees the ports the
	// service was given, before any bind verification has happened.
	if allocations, listErr := sv.sup.deps.Ports.ServicePorts(sv.projectID, sv.name); listErr == nil {
		sv.mu.Lock()
		sv.ports = allocations
		sv.mu.Unlock()
	}

	_ = sv.sup.deps.DB.InsertInstance(storage.InstanceRecord{
		ID:           instanceID,
		ProjectID:    sv.projectID,
		ServiceName:  sv.name,
		PID:          handle.PID(),
		Status:       string(dto.StatusRunning),
		Runtime:      string(spec.Runtime),
		CommandLine:  displayLine,
		CWD:          plan.Dir,
		StartedAt:    now,
		RestartCount: sv.currentRestarts(),
	})

	sv.emit(dto.EventServiceStarted, sv.name+" started", map[string]any{
		"pid":     handle.PID(),
		"command": displayLine,
		"runtime": string(spec.Runtime),
	})
	for name, port := range plan.Ports.ByName {
		sv.emit(dto.EventPortReserved, fmt.Sprintf("%s reserved port %d", sv.name, port),
			map[string]any{"port": port, "port_name": name})
	}

	sv.startHealthMonitor(plan.Health, handle)
	go sv.verifyPorts(plan)
	go sv.watch(handle, generation, plan)
	return nil
}

// logWriters wires the service's output into the log store. A log failure never
// prevents a start: losing output is bad, refusing to run the service is worse.
func (sv *service) logWriters() (io.Writer, io.Writer) {
	if sv.sup.deps.Logs == nil {
		return nil, nil
	}
	serviceLog, err := sv.sup.deps.Logs.Service(sv.projectID, sv.name)
	if err != nil {
		return nil, nil
	}
	return serviceLog.Writer(logstore.StreamStdout), serviceLog.Writer(logstore.StreamStderr)
}

func (sv *service) currentRestarts() int {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	return sv.restarts
}

// startHealthMonitor begins probing and publishes only transitions, so a
// healthy service does not generate an event every interval.
func (sv *service) startHealthMonitor(spec health.Spec, handle runtime.Handle) {
	monitor := health.NewMonitor(spec, handle.Running, func(result health.Result) {
		sv.emit(dto.EventHealthChanged,
			fmt.Sprintf("%s health is %s", sv.name, result.Status),
			map[string]any{
				"status":  string(result.Status),
				"target":  result.Target,
				"message": result.Message,
			})
	})
	sv.mu.Lock()
	previous := sv.monitor
	sv.monitor = monitor
	sv.mu.Unlock()
	if previous != nil {
		previous.Stop()
	}
	monitor.Start(sv.sup.ctx)
}

// verifyPorts records whether the service actually listened on what it was
// given. A service that ignores its PORT variable is reported as UNVERIFIED and
// left alone: DevMan does not kill a running process over a warning.
func (sv *service) verifyPorts(plan launchPlan) {
	if len(plan.Ports.Records) == 0 {
		return
	}
	wait := plan.StartWait
	if wait <= 0 || wait > maxBindWait {
		wait = maxBindWait
	}
	bound, err := sv.sup.deps.Ports.WaitForBind(sv.projectID, sv.name, wait)
	if err != nil {
		return
	}
	if allocations, listErr := sv.sup.deps.Ports.ServicePorts(sv.projectID, sv.name); listErr == nil {
		sv.mu.Lock()
		sv.ports = allocations
		sv.mu.Unlock()
	}
	if !sv.running() {
		return
	}
	if bound {
		for name, port := range plan.Ports.ByName {
			sv.emit(dto.EventPortBound, fmt.Sprintf("%s is listening on %d", sv.name, port),
				map[string]any{"port": port, "port_name": name})
		}
		return
	}
	sv.log("warning: the service did not bind every port DevMan allocated; " +
		"the ports remain reserved and are reported as UNVERIFIED")
}

// watch turns process exit into state. It is the only place that decides
// whether an exit was expected.
func (sv *service) watch(handle runtime.Handle, generation uint64, plan launchPlan) {
	select {
	case <-handle.Done():
	case <-sv.sup.ctx.Done():
		return
	}

	status, _ := handle.Exit()

	sv.mu.Lock()
	if sv.generation != generation {
		// A stop or a restart already handled this instance.
		sv.mu.Unlock()
		return
	}
	code := status.Code
	desired := sv.desired
	startedAt := sv.startedAt
	instanceID := sv.instanceID
	sv.lastExit = &code
	sv.handle = nil
	sv.logCapture = dto.LogCaptureNone
	sv.stoppedAt = time.Now().UTC()
	if desired == dto.DesiredRunning {
		sv.actual = dto.StatusCrashed
	} else {
		sv.actual = dto.StatusStopped
	}
	// A service that ran long enough to be useful starts its restart budget
	// afresh, so a nightly crash never exhausts max_attempts.
	if !startedAt.IsZero() && time.Since(startedAt) >= stableRunDuration {
		sv.restarts = 0
	}
	sv.mu.Unlock()

	sv.stopMonitor()
	sv.finishInstance(instanceID, status, desired)
	sv.releasePorts()
	sv.persist()

	if desired != dto.DesiredRunning {
		sv.log(fmt.Sprintf("stopped (exit code %d)", code))
		sv.emit(dto.EventServiceStopped, sv.name+" stopped",
			map[string]any{"exit_code": code})
		return
	}

	sv.log(fmt.Sprintf("exited unexpectedly with code %d", code))
	sv.emit(dto.EventServiceCrashed,
		fmt.Sprintf("%s exited unexpectedly with code %d", sv.name, code),
		map[string]any{"exit_code": code, "signal": status.Signal})
	sv.maybeRestart(plan, status)
}

func (sv *service) finishInstance(instanceID string, status platform.ExitStatus, desired dto.DesiredState) {
	if instanceID == "" {
		return
	}
	final := string(dto.StatusStopped)
	if desired == dto.DesiredRunning {
		final = string(dto.StatusCrashed)
	}
	code := status.Code
	stoppedAt := status.ExitedAt
	if stoppedAt.IsZero() {
		stoppedAt = time.Now().UTC()
	}
	_ = sv.sup.deps.DB.FinishInstance(instanceID, final, &code, stoppedAt)
}

func (sv *service) releasePorts() {
	if sv.sup.deps.Ports == nil {
		return
	}
	if err := sv.sup.deps.Ports.ReleaseService(sv.projectID, sv.name); err != nil {
		return
	}
	sv.mu.Lock()
	released := sv.ports
	sv.ports = nil
	sv.mu.Unlock()
	for _, allocation := range released {
		sv.emit(dto.EventPortReleased, fmt.Sprintf("released port %d", allocation.Port),
			map[string]any{"port": allocation.Port, "port_name": allocation.Name})
	}
}

// maybeRestart applies the restart policy. It acts only while the desired state
// is RUNNING, which is re-checked after the backoff delay: a stop issued while
// DevMan was waiting must win.
func (sv *service) maybeRestart(plan launchPlan, status platform.ExitStatus) {
	switch plan.Restart.Policy {
	case config.RestartAlways:
	case config.RestartOnFailure:
		if status.Code == 0 {
			return
		}
	default:
		return
	}

	sv.mu.Lock()
	attempt := sv.restarts
	sv.mu.Unlock()

	if plan.Restart.MaxAttempts > 0 && attempt >= plan.Restart.MaxAttempts {
		sv.setStatus(dto.StatusFailed,
			fmt.Sprintf("gave up after %d restart attempts", attempt),
			errs.New(errs.CodeProcessCrashed,
				"%s kept exiting; gave up after %d attempts", sv.name, attempt).
				With("attempts", attempt))
		sv.log(fmt.Sprintf("not restarting: reached max_attempts (%d)", plan.Restart.MaxAttempts))
		return
	}

	delay := backoff(plan.Restart, attempt)
	sv.emit(dto.EventServiceRestartScheduled,
		fmt.Sprintf("restarting %s in %s", sv.name, delay.Round(time.Millisecond)),
		map[string]any{"attempt": attempt + 1, "delay_ms": delay.Milliseconds()})
	sv.log(fmt.Sprintf("restarting in %s (attempt %d)", delay.Round(time.Millisecond), attempt+1))

	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-sv.sup.ctx.Done():
		return
	case <-timer.C:
	}

	sv.mu.Lock()
	desired := sv.desired
	sv.restarts = attempt + 1
	sv.mu.Unlock()
	if desired != dto.DesiredRunning {
		return
	}
	if err := sv.sup.startTracked(sv, true); err != nil {
		sv.log("restart failed: " + err.Error())
	}
}

// backoff is exponential with a cap and jitter. The jitter matters when a
// dependency such as a database comes back: without it every dependent service
// retries in lockstep.
func backoff(policy restartPolicy, attempt int) time.Duration {
	base := policy.Delay
	if base <= 0 {
		base = time.Second
	}
	maxDelay := policy.MaxDelay
	if maxDelay <= 0 {
		maxDelay = 30 * time.Second
	}
	delay := base
	for i := 0; i < attempt && delay < maxDelay; i++ {
		delay *= 2
	}
	if delay > maxDelay {
		delay = maxDelay
	}
	// Up to 20% of jitter, never negative so the delay is always honoured.
	jitter := time.Duration(rand.Int63n(int64(delay/5) + 1))
	return delay + jitter
}

// fail records a start failure, distinguishing "a precondition is missing" from
// "the attempt broke".
func (sv *service) fail(err error) {
	de := errs.From(err)
	status := dto.StatusFailed
	event := dto.EventServiceExited
	if blockedCode(de.Code) {
		status = dto.StatusBlocked
		event = dto.EventServiceBlocked
	}
	sv.setStatus(status, de.Message, de)
	sv.emit(event, de.Message, map[string]any{"code": string(de.Code)})
}

// stopInstance shuts the current instance down. The caller must hold sv.opMu.
func (sv *service) stopInstance(graceful time.Duration) error {
	// The desired state is written before anything is signalled, so a crash of
	// the daemon mid-stop cannot leave a service that gets auto-restarted.
	sv.setDesired(dto.DesiredStopped)

	sv.mu.Lock()
	handle := sv.handle
	instanceID := sv.instanceID
	// The watcher of this instance is invalidated: this function reports the
	// exit itself, so the stop is never mistaken for a crash.
	sv.generation++
	if handle != nil {
		sv.actual = dto.StatusStopping
	}
	sv.mu.Unlock()

	if handle == nil {
		sv.stopMonitor()
		sv.releasePorts()
		sv.setStatus(dto.StatusStopped, "", nil)
		return nil
	}

	sv.emit(dto.EventServiceStopping, "stopping "+sv.name, nil)
	sv.stopMonitor()

	outcome := handle.Stop(graceful)
	code := outcome.Status.Code

	sv.mu.Lock()
	sv.handle = nil
	sv.lastExit = &code
	sv.actual = dto.StatusStopped
	sv.message = ""
	sv.reason = nil
	sv.logCapture = dto.LogCaptureNone
	sv.stoppedAt = time.Now().UTC()
	sv.mu.Unlock()

	if outcome.Graceful {
		sv.log(fmt.Sprintf("stopped gracefully (exit code %d)", code))
	} else {
		reason := "did not exit within the graceful timeout"
		if !outcome.SignalSent {
			reason = "no graceful signal could be delivered"
			if outcome.SignalError != "" {
				reason += ": " + outcome.SignalError
			}
		}
		sv.log("force killed the process tree: " + reason)
	}

	sv.finishInstance(instanceID, outcome.Status, dto.DesiredStopped)
	sv.releasePorts()
	sv.persist()
	sv.emit(dto.EventServiceStopped, sv.name+" stopped", map[string]any{
		"exit_code": code,
		"graceful":  outcome.Graceful,
	})
	return nil
}
