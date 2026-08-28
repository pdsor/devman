// Package supervisor owns the lifecycle of every service DevMan manages.
//
// Two ideas shape this package:
//
// Desired state is stored separately from actual state. A restart policy only
// acts while the desired state is RUNNING, so `devman stop` can never lose a
// race against an automatic restart, and a daemon restart cannot resurrect a
// service the user deliberately stopped.
//
// Nothing here knows how a service is executed. Host processes, compose
// services and external services all arrive through internal/runtime, which is
// also why "stop" means something different for each of them without this
// package containing a single OS or Docker branch.
package supervisor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"github.com/devman-project/devman/internal/events"
	"github.com/devman-project/devman/internal/health"
	"github.com/devman-project/devman/internal/logstore"
	"github.com/devman-project/devman/internal/portmgr"
	"github.com/devman-project/devman/internal/registry"
	"github.com/devman-project/devman/internal/runtime"
	"github.com/devman-project/devman/internal/settings"
	"github.com/devman-project/devman/internal/storage"
	"github.com/devman-project/devman/pkg/config"
	"github.com/devman-project/devman/pkg/dto"
	"github.com/devman-project/devman/pkg/errs"
)

// stableRunDuration is how long a service must stay up before its restart
// counter is forgotten. Without it a service that is restarted once a day would
// eventually exhaust max_attempts for no good reason.
const stableRunDuration = 60 * time.Second

// Deps are the collaborators the supervisor needs. They are injected so tests
// can substitute a temporary database, a fake port prober and an in-memory log
// directory.
type Deps struct {
	DB       *storage.DB
	Registry *registry.Registry
	Ports    *portmgr.Manager
	Logs     *logstore.Manager
	Events   *events.Bus
	Runtimes runtime.Set
	// Settings is a function so a live edit of config.yaml is picked up without
	// restarting the daemon.
	Settings func() *settings.Settings
}

// Supervisor manages services across all registered projects.
type Supervisor struct {
	deps Deps

	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	services map[string]*service

	// usage samples what the running services cost. It is owned here because
	// only the supervisor knows which pids are currently services.
	usage *usageSampler
}

// New creates a supervisor. Call Close to shut down monitoring goroutines;
// stopping services is a separate, explicit decision.
func New(deps Deps) *Supervisor {
	if deps.Settings == nil {
		defaults := settings.Default()
		deps.Settings = func() *settings.Settings { return defaults }
	}
	if deps.Runtimes.Host == nil {
		deps.Runtimes = runtime.NewSet()
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Supervisor{
		deps:     deps,
		ctx:      ctx,
		cancel:   cancel,
		services: map[string]*service{},
		usage:    newUsageSampler(hostSource{}),
	}
	// Resource sampling is tied to the supervisor's context, so Close stops it
	// the same way it stops health monitoring.
	go s.usage.run(ctx, s.usageRoots)
	return s
}

// usageRoots is the pid of every service the sampler should measure. A service
// with no host process — compose, external, or simply stopped — has no tree and
// is left out, which is what makes its usage absent rather than zero.
func (s *Supervisor) usageRoots() map[string]int {
	s.mu.Lock()
	list := make([]*service, 0, len(s.services))
	keys := make([]string, 0, len(s.services))
	for key, sv := range s.services {
		list = append(list, sv)
		keys = append(keys, key)
	}
	s.mu.Unlock()

	roots := make(map[string]int, len(list))
	for i, sv := range list {
		sv.mu.Lock()
		handle := sv.handle
		sv.mu.Unlock()
		if handle == nil || !handle.Running() {
			continue
		}
		if pid := handle.PID(); pid > 0 {
			roots[keys[i]] = pid
		}
	}
	return roots
}

// MachineUsage is the last whole-machine reading.
func (s *Supervisor) MachineUsage() dto.MachineUsage { return s.usage.machineUsage() }

// Close stops health monitoring and pending restart timers.
//
// It does not terminate services: stopping them is a separate, explicit
// decision that the daemon makes with StopAll before shutting down. Keeping the
// two apart means a caller can tear down the supervisor without implying
// anything about the processes it was watching.
func (s *Supervisor) Close() {
	s.cancel()
	s.mu.Lock()
	list := make([]*service, 0, len(s.services))
	for _, sv := range s.services {
		list = append(list, sv)
	}
	s.mu.Unlock()
	for _, sv := range list {
		sv.stopMonitor()
	}
}

func serviceKey(projectID, name string) string { return projectID + "/" + name }

// service is the live state of one service.
type service struct {
	sup       *Supervisor
	projectID string
	name      string

	mu      sync.Mutex
	desired dto.DesiredState
	actual  dto.ProcessStatus
	kind    config.RuntimeKind

	handle  runtime.Handle
	monitor *health.Monitor
	// generation invalidates the watcher of a previous instance. A stop
	// increments it before terminating, so the exit it causes is never mistaken
	// for a crash.
	generation uint64

	instanceID  string
	commandLine string
	cwd         string
	startedAt   time.Time
	stoppedAt   time.Time
	restarts    int
	lastExit    *int
	logCapture  dto.LogCapture
	adopted     bool
	message     string
	reason      *errs.Error
	ports       []dto.PortAllocation
	healthSpec  health.Spec

	// opMu serialises start/stop/restart for this service so two concurrent
	// requests cannot interleave. State reads use mu and never block on it.
	opMu sync.Mutex
}

// serviceState returns the tracked state of a service, creating it on demand.
func (s *Supervisor) serviceState(projectID, name string) *service {
	key := serviceKey(projectID, name)
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.services[key]; ok {
		return existing
	}
	sv := &service{
		sup:        s,
		projectID:  projectID,
		name:       name,
		desired:    dto.DesiredStopped,
		actual:     dto.StatusStopped,
		logCapture: dto.LogCaptureNone,
	}
	// A previous daemon may have recorded state for this service; load it so a
	// desired state of RUNNING survives a restart.
	if record, err := s.deps.DB.ServiceRuntime(projectID, name); err == nil {
		sv.desired = dto.DesiredState(record.DesiredState)
		sv.actual = dto.ProcessStatus(record.ActualState)
		sv.restarts = record.RestartCount
		sv.lastExit = record.LastExitCode
		sv.logCapture = dto.LogCapture(record.LogCapture)
		sv.adopted = record.Adopted
		sv.instanceID = record.InstanceID
		if record.SpawnedAt != nil {
			sv.startedAt = *record.SpawnedAt
		}
		// A process this daemon does not supervise is not running as far as the
		// in-memory view is concerned; reconciliation decides otherwise.
		if sv.actual == dto.StatusRunning || sv.actual == dto.StatusStarting {
			sv.actual = dto.StatusUnknown
		}
	}
	s.services[key] = sv
	return sv
}

// known returns the tracked state without creating it.
func (s *Supervisor) known(projectID, name string) (*service, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sv, ok := s.services[serviceKey(projectID, name)]
	return sv, ok
}

func (sv *service) snapshot() service {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	// Copied without the mutexes: callers only read the value fields.
	return service{
		projectID:   sv.projectID,
		name:        sv.name,
		desired:     sv.desired,
		actual:      sv.actual,
		kind:        sv.kind,
		instanceID:  sv.instanceID,
		commandLine: sv.commandLine,
		cwd:         sv.cwd,
		startedAt:   sv.startedAt,
		stoppedAt:   sv.stoppedAt,
		restarts:    sv.restarts,
		lastExit:    sv.lastExit,
		logCapture:  sv.logCapture,
		adopted:     sv.adopted,
		message:     sv.message,
		reason:      sv.reason,
		ports:       append([]dto.PortAllocation(nil), sv.ports...),
		healthSpec:  sv.healthSpec,
	}
}

// running reports whether this daemon is currently supervising a live instance.
func (sv *service) running() bool {
	sv.mu.Lock()
	handle := sv.handle
	sv.mu.Unlock()
	return handle != nil && handle.Running()
}

func (sv *service) setStatus(status dto.ProcessStatus, message string, reason *errs.Error) {
	sv.mu.Lock()
	sv.actual = status
	sv.message = message
	sv.reason = reason
	sv.mu.Unlock()
	sv.persist()
}

func (sv *service) setDesired(state dto.DesiredState) {
	sv.mu.Lock()
	sv.desired = state
	sv.mu.Unlock()
	sv.persist()
}

// persist writes the runtime state to SQLite. Desired and actual state are
// written together so they can never disagree across a daemon restart.
func (sv *service) persist() {
	sv.mu.Lock()
	record := storage.ServiceRuntimeRecord{
		ProjectID:    sv.projectID,
		ServiceName:  sv.name,
		DesiredState: string(sv.desired),
		ActualState:  string(sv.actual),
		InstanceID:   sv.instanceID,
		RestartCount: sv.restarts,
		LastExitCode: sv.lastExit,
		LogCapture:   string(sv.logCapture),
		Adopted:      sv.adopted,
	}
	if sv.handle != nil {
		identity := sv.handle.Identity()
		record.PID = identity.PID
		record.Executable = identity.Executable
		record.CommandFingerprint = identity.Fingerprint
		if !identity.SpawnedAt.IsZero() {
			spawned := identity.SpawnedAt
			record.SpawnedAt = &spawned
		}
	} else if !sv.startedAt.IsZero() && sv.actual == dto.StatusRunning {
		started := sv.startedAt
		record.SpawnedAt = &started
	}
	sv.mu.Unlock()

	_ = sv.sup.deps.DB.UpsertServiceRuntime(record)
}

func (sv *service) emit(kind dto.EventType, message string, data map[string]any) {
	if sv.sup.deps.Events == nil {
		return
	}
	sv.sup.deps.Events.Emit(kind, sv.projectID, sv.name, message, data)
}

// log writes a DevMan annotation into the service log, so the reason a service
// stopped sits next to the output that preceded it.
func (sv *service) log(message string) {
	if sv.sup.deps.Logs == nil {
		return
	}
	serviceLog, err := sv.sup.deps.Logs.Service(sv.projectID, sv.name)
	if err != nil {
		return
	}
	serviceLog.Append(logstore.StreamSystem, message)
}

func (sv *service) stopMonitor() {
	sv.mu.Lock()
	monitor := sv.monitor
	sv.monitor = nil
	sv.mu.Unlock()
	if monitor != nil {
		monitor.Stop()
	}
}

// healthResult reports the current health, defaulting to N/A for services with
// process-only health and UNKNOWN for ones that were never started.
func (sv *service) healthResult() dto.HealthResult {
	sv.mu.Lock()
	monitor := sv.monitor
	spec := sv.healthSpec
	status := sv.actual
	sv.mu.Unlock()

	if monitor == nil {
		kind := spec.Type
		if kind == "" {
			kind = config.HealthProcess
		}
		result := dto.HealthResult{Status: dto.HealthUnknown, Type: string(kind), Target: spec.Target()}
		if status != dto.StatusRunning {
			result.Status = dto.HealthUnknown
		}
		return result
	}
	return monitor.Current().DTO(spec.Type)
}

// newInstanceID produces an identifier for one run of a service.
func newInstanceID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "i_" + hex.EncodeToString([]byte(time.Now().UTC().Format(time.RFC3339Nano)))[:16]
	}
	return "i_" + hex.EncodeToString(buf)
}

// blockedCode reports whether an error means "a precondition is missing"
// rather than "the attempt failed". Blocked services are reported distinctly
// because nothing is broken: install Docker, set the variable, free the port.
func blockedCode(code errs.Code) bool {
	switch code {
	case errs.CodeServiceBlocked,
		errs.CodeDockerNotFound,
		errs.CodeDockerUnavailable,
		errs.CodeCommandNotFound,
		errs.CodeEnvMissing,
		errs.CodePortConflict,
		errs.CodePortExhausted,
		errs.CodeProjectUntrusted:
		return true
	default:
		return false
	}
}
