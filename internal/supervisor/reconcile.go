package supervisor

import (
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/devman-project/devman/internal/envresolve"
	"github.com/devman-project/devman/internal/health"
	"github.com/devman-project/devman/internal/platform"
	"github.com/devman-project/devman/internal/runtime"
	"github.com/devman-project/devman/internal/storage"
	"github.com/devman-project/devman/pkg/config"
	"github.com/devman-project/devman/pkg/dto"
)

// ReconcileResult reports what a freshly started daemon found.
type ReconcileResult struct {
	// Adopted are services that outlived the previous daemon.
	Adopted []dto.Service
	// Vanished are services the previous daemon believed were running and that
	// are gone.
	Vanished []dto.Service
}

// Reconcile brings the in-memory view in line with reality after a daemon
// restart or crash.
//
// Two outcomes are possible for each service the previous daemon believed was
// running. If the process is still there, it is adopted: it keeps
// `status: RUNNING` and gains `observability.log_capture: detached`, because its
// output pipes died with the daemon that created them. If it is gone, it is
// recorded as CRASHED (when it was meant to be running) and its ports are
// released.
//
// A vanished service is never auto-started. Reconciliation reports the truth; it
// does not make decisions the user did not ask for.
//
// On Windows the first outcome is rare by design: services run inside a Job
// Object created with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE, so a daemon that dies
// takes its service trees with it rather than leaving orphans behind. There,
// reconciliation normally records the second outcome and releases the ports.
func (s *Supervisor) Reconcile() (ReconcileResult, error) {
	records, err := s.deps.DB.ServiceRuntimes("")
	if err != nil {
		return ReconcileResult{}, err
	}

	var result ReconcileResult
	for _, record := range records {
		if !wasSupervised(record.ActualState) {
			continue
		}
		sv := s.serviceState(record.ProjectID, record.ServiceName)
		spec := s.specFor(record.ProjectID, record.ServiceName)

		if s.adoptable(record) {
			s.adopt(sv, spec, record)
			result.Adopted = append(result.Adopted, s.serviceDTO(sv, spec))
			continue
		}
		s.markVanished(sv, record)
		result.Vanished = append(result.Vanished, s.serviceDTO(sv, spec))
	}
	return result, nil
}

// wasSupervised reports whether the previous daemon thought it owned a process.
func wasSupervised(state string) bool {
	switch dto.ProcessStatus(state) {
	case dto.StatusRunning, dto.StatusStarting, dto.StatusStopping, dto.StatusUnknown:
		return true
	default:
		return false
	}
}

// adoptable decides whether the recorded PID is still the recorded process.
// A PID alone is never enough: PIDs are recycled, so the spawn time and
// executable have to agree too.
func (s *Supervisor) adoptable(record storage.ServiceRuntimeRecord) bool {
	if record.PID == 0 {
		return false
	}
	if !platform.Alive(record.PID) {
		return false
	}
	info, err := platform.Inspect(record.PID)
	if err != nil {
		// Alive but not inspectable: trust liveness rather than discard a
		// running service over a platform limitation.
		return true
	}
	return info.MatchesIdentity(identityOf(record))
}

func identityOf(record storage.ServiceRuntimeRecord) platform.Identity {
	identity := platform.Identity{
		PID:         record.PID,
		Executable:  record.Executable,
		Fingerprint: record.CommandFingerprint,
	}
	if record.SpawnedAt != nil {
		identity.SpawnedAt = *record.SpawnedAt
	}
	return identity
}

// specFor looks up a service declaration, tolerating a project whose config has
// since become unreadable: a running process still has to be reported.
func (s *Supervisor) specFor(projectID, name string) *config.Service {
	cfg, err := s.deps.Registry.Config(projectID)
	if err != nil {
		return nil
	}
	spec, err := cfg.Service(name)
	if err != nil {
		return nil
	}
	return spec
}

// adopt re-attaches to a surviving process.
func (s *Supervisor) adopt(sv *service, spec *config.Service, record storage.ServiceRuntimeRecord) {
	identity := identityOf(record)
	commandLine := s.lastCommandLine(record)
	handle := runtime.Adopted(identity, commandLine)

	plan := launchPlan{}
	healthSpec := health.Spec{Type: config.HealthProcess}
	if spec != nil {
		plan.Restart = resolveRestart(spec.Restart, s.deps.Settings())
		plan.Graceful = spec.GracefulTimeout.Or(s.deps.Settings().Defaults.GracefulTimeout.Duration)
		if resolved, err := s.healthSpecFor(record.ProjectID, spec); err == nil {
			healthSpec = resolved
		}
		sv.mu.Lock()
		sv.kind = spec.Runtime
		sv.mu.Unlock()
	}
	plan.Health = healthSpec

	sv.mu.Lock()
	sv.generation++
	generation := sv.generation
	sv.handle = handle
	sv.actual = dto.StatusRunning
	sv.desired = dto.DesiredState(record.DesiredState)
	if sv.desired == "" {
		sv.desired = dto.DesiredRunning
	}
	sv.adopted = true
	// The pipes belonged to the previous daemon, so output is no longer being
	// captured. Saying so is more useful than inventing a new status.
	sv.logCapture = dto.LogCaptureDetached
	sv.instanceID = record.InstanceID
	sv.restarts = record.RestartCount
	sv.commandLine = commandLine
	sv.healthSpec = healthSpec
	if record.SpawnedAt != nil {
		sv.startedAt = *record.SpawnedAt
	}
	sv.message = "adopted after a daemon restart; log capture is detached until this service is restarted"
	sv.reason = nil
	sv.mu.Unlock()
	sv.persist()

	// The ports are still held in the registry by the surviving process; confirm
	// which of them are really bound.
	if _, err := s.deps.Ports.Verify(record.ProjectID, record.ServiceName); err == nil {
		if allocations, listErr := s.deps.Ports.ServicePorts(record.ProjectID, record.ServiceName); listErr == nil {
			sv.mu.Lock()
			sv.ports = allocations
			sv.mu.Unlock()
		}
	}

	sv.log("adopted by a new daemon (pid " + fmt.Sprint(record.PID) + "); log capture is detached")
	sv.emit(dto.EventServiceAdopted,
		fmt.Sprintf("adopted %s (pid %d) after a daemon restart", sv.name, record.PID),
		map[string]any{"pid": record.PID, "log_capture": string(dto.LogCaptureDetached)})

	sv.startHealthMonitor(healthSpec, handle)
	go sv.watch(handle, generation, plan)
}

// markVanished records a service the previous daemon believed was running and
// that is no longer there.
func (s *Supervisor) markVanished(sv *service, record storage.ServiceRuntimeRecord) {
	desired := dto.DesiredState(record.DesiredState)
	status := dto.StatusStopped
	if desired == dto.DesiredRunning {
		status = dto.StatusCrashed
	}

	sv.mu.Lock()
	sv.handle = nil
	sv.actual = status
	sv.desired = desired
	sv.adopted = false
	sv.logCapture = dto.LogCaptureNone
	sv.stoppedAt = time.Now().UTC()
	instanceID := sv.instanceID
	sv.mu.Unlock()

	if instanceID != "" {
		_ = s.deps.DB.FinishInstance(instanceID, string(status), nil, time.Now().UTC())
	}
	sv.releasePorts()
	sv.persist()

	sv.log("was not running when the daemon started again")
	sv.emit(dto.EventServiceExited,
		fmt.Sprintf("%s was gone when the daemon restarted", sv.name),
		map[string]any{"previous_pid": record.PID})
}

// lastCommandLine recovers what the surviving process was started with, so
// status output stays informative for an adopted service.
func (s *Supervisor) lastCommandLine(record storage.ServiceRuntimeRecord) string {
	instances, err := s.deps.DB.Instances(record.ProjectID, record.ServiceName, 1)
	if err != nil || len(instances) == 0 {
		return record.Executable
	}
	if instances[0].CommandLine == "" {
		return record.Executable
	}
	return instances[0].CommandLine
}

// healthSpecFor resolves a health probe for a service that is already running,
// using the ports it currently holds. Nothing is reserved and no process is
// touched.
func (s *Supervisor) healthSpecFor(projectID string, spec *config.Service) (health.Spec, error) {
	cfg, err := s.deps.Registry.Config(projectID)
	if err != nil {
		return health.Spec{}, err
	}
	current := s.deps.Settings()
	platformKey := config.CurrentPlatform()
	dir := spec.AbsCWD(cfg.ProjectRoot, platformKey)

	base := envresolve.CurrentEnv()
	files, err := envresolve.LoadFiles(cfg.ProjectRoot, spec.EnvFile)
	if err != nil {
		return health.Spec{}, err
	}
	userEnv := envresolve.Layers{
		Base:    base,
		Files:   files,
		Service: spec.Execution(platformKey).Env,
	}.UserEnv()

	ports := map[string]int{}
	if allocations, listErr := s.deps.Ports.ServicePorts(projectID, spec.Name); listErr == nil {
		for _, allocation := range allocations {
			ports[allocation.Name] = allocation.Port
		}
	}
	home, _ := os.UserHomeDir()
	defaultPort, _ := spec.PrimaryPortName()

	expand := config.TemplateContext{
		ProjectDir:      cfg.ProjectRoot,
		ServiceDir:      dir,
		Home:            home,
		Ports:           ports,
		DefaultPortName: defaultPort,
		Env: func(name string) (string, bool) {
			value, ok := userEnv[name]
			return value, ok
		},
	}
	return health.SpecFrom(spec.Health, expand.Expand, health.Defaults{
		Interval: current.Defaults.HealthInterval.Duration,
		Timeout:  current.Defaults.HealthTimeout.Duration,
		Retries:  current.Defaults.HealthRetries,
	})
}

// StopAll stops every service this daemon supervises.
//
// `devman daemon stop` uses it: the daemon exiting means nothing DevMan started
// is left behind. Services are stopped in parallel because each one has its own
// graceful timeout and a serial sweep would multiply them.
func (s *Supervisor) StopAll() []dto.Service {
	s.mu.Lock()
	list := make([]*service, 0, len(s.services))
	for _, sv := range s.services {
		list = append(list, sv)
	}
	s.mu.Unlock()

	results := make([]dto.Service, len(list))
	var wg sync.WaitGroup
	for i, sv := range list {
		if !sv.running() {
			results[i] = s.serviceDTO(sv, s.specFor(sv.projectID, sv.name))
			continue
		}
		wg.Add(1)
		go func(index int, target *service) {
			defer wg.Done()
			spec := s.specFor(target.projectID, target.name)
			target.opMu.Lock()
			_ = target.stopInstance(s.gracefulTimeout(spec))
			target.opMu.Unlock()
			results[index] = s.serviceDTO(target, spec)
		}(i, sv)
	}
	wg.Wait()
	return results
}

// RunningCount reports how many services this daemon currently supervises.
func (s *Supervisor) RunningCount() int {
	s.mu.Lock()
	list := make([]*service, 0, len(s.services))
	for _, sv := range s.services {
		list = append(list, sv)
	}
	s.mu.Unlock()

	count := 0
	for _, sv := range list {
		if sv.running() {
			count++
		}
	}
	return count
}
