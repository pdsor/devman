package supervisor

import (
	"fmt"
	"time"

	"github.com/devman-project/devman/internal/registry"
	"github.com/devman-project/devman/pkg/config"
	"github.com/devman-project/devman/pkg/dto"
	"github.com/devman-project/devman/pkg/errs"
)

// serviceConfig re-reads devman.yaml and checks trust.
//
// The file is read on every start and restart, so editing devman.yaml and
// restarting a service is all a user ever has to do; there is no reload command
// to remember. The registry caches by mtime, so ordinary status calls stay
// cheap.
func (s *Supervisor) serviceConfig(projectID, name string) (*config.Config, *config.Service, error) {
	if err := s.deps.Registry.EnsureTrusted(projectID); err != nil {
		return nil, nil, err
	}
	cfg, err := s.deps.Registry.ValidConfig(projectID)
	if err != nil {
		return nil, nil, err
	}
	spec, err := cfg.Service(name)
	if err != nil {
		return nil, nil, err
	}
	return cfg, spec, nil
}

// startTracked starts an already tracked service, re-reading its configuration.
func (s *Supervisor) startTracked(sv *service, isRestart bool) error {
	sv.opMu.Lock()
	defer sv.opMu.Unlock()
	cfg, spec, err := s.serviceConfig(sv.projectID, sv.name)
	if err != nil {
		sv.fail(err)
		return err
	}
	return sv.launch(cfg, spec, isRestart)
}

// StartService starts one service.
func (s *Supervisor) StartService(projectID, name string) (dto.Service, error) {
	cfg, spec, err := s.serviceConfig(projectID, name)
	if err != nil {
		return dto.Service{}, err
	}
	sv := s.serviceState(projectID, name)

	sv.opMu.Lock()
	launchErr := sv.launch(cfg, spec, false)
	sv.opMu.Unlock()

	return s.serviceDTO(sv, spec), launchErr
}

// StopService stops one service and records that it must stay stopped.
func (s *Supervisor) StopService(projectID, name string) (dto.Service, error) {
	cfg, err := s.deps.Registry.Config(projectID)
	var spec *config.Service
	if err == nil {
		spec, _ = cfg.Service(name)
	}
	sv, tracked := s.known(projectID, name)
	if !tracked {
		// Nothing was ever started, but the desired state still has to be
		// recorded so reconciliation does not resurrect a stale process.
		sv = s.serviceState(projectID, name)
	}

	sv.opMu.Lock()
	graceful := s.gracefulTimeout(spec)
	stopErr := sv.stopInstance(graceful)
	sv.opMu.Unlock()

	return s.serviceDTO(sv, spec), stopErr
}

// RestartService stops and starts a service, re-reading its configuration in
// between so an edit takes effect.
func (s *Supervisor) RestartService(projectID, name string) (dto.Service, error) {
	cfg, spec, err := s.serviceConfig(projectID, name)
	if err != nil {
		return dto.Service{}, err
	}
	sv := s.serviceState(projectID, name)

	sv.opMu.Lock()
	defer sv.opMu.Unlock()

	if stopErr := sv.stopInstance(s.gracefulTimeout(spec)); stopErr != nil {
		return s.serviceDTO(sv, spec), stopErr
	}
	sv.mu.Lock()
	sv.restarts = 0
	sv.mu.Unlock()

	launchErr := sv.launch(cfg, spec, false)
	return s.serviceDTO(sv, spec), launchErr
}

func (s *Supervisor) gracefulTimeout(spec *config.Service) time.Duration {
	fallback := s.deps.Settings().Defaults.GracefulTimeout.Duration
	if spec == nil {
		return fallback
	}
	return spec.GracefulTimeout.Or(fallback)
}

// StartProject starts a set of services in dependency order.
//
// One service failing does not abort the rest: a broken worker should not stop
// the frontend from coming up. Services that depend on the failure are reported
// as DEPENDENCY_FAILED instead of being launched into a broken environment.
func (s *Supervisor) StartProject(
	projectID string, names []string, profile string, all bool,
) (dto.OperationResult, error) {
	record, err := s.deps.Registry.Project(projectID)
	if err != nil {
		return dto.OperationResult{}, err
	}
	if err := s.deps.Registry.EnsureTrusted(projectID); err != nil {
		return dto.OperationResult{}, err
	}
	cfg, err := s.deps.Registry.ValidConfig(projectID)
	if err != nil {
		return dto.OperationResult{}, err
	}
	selected, err := cfg.ResolveServiceSet(names, profile, all)
	if err != nil {
		return dto.OperationResult{}, err
	}
	// TopoOrder also pulls in transitive dependencies: starting `backend` has to
	// start the database it declares, or the start is a lie.
	ordered, err := cfg.TopoOrder(selected)
	if err != nil {
		return dto.OperationResult{}, err
	}

	result := dto.OperationResult{Project: projectID}
	for _, name := range ordered {
		spec, specErr := cfg.Service(name)
		if specErr != nil {
			result.Errors = append(result.Errors, *dto.FromError(specErr))
			continue
		}
		sv := s.serviceState(projectID, name)

		if depErr := s.awaitDependencies(projectID, spec); depErr != nil {
			sv.fail(depErr)
			result.Errors = append(result.Errors, *dto.FromError(depErr))
			result.Services = append(result.Services, s.serviceDTO(sv, spec))
			continue
		}

		if sv.running() {
			result.Services = append(result.Services, s.serviceDTO(sv, spec))
			continue
		}

		sv.opMu.Lock()
		launchErr := sv.launch(cfg, spec, false)
		sv.opMu.Unlock()
		if launchErr != nil {
			result.Errors = append(result.Errors, *dto.FromError(launchErr))
		}
		result.Services = append(result.Services, s.serviceDTO(sv, spec))
	}

	_ = s.deps.DB.TouchProjectStarted(projectID, time.Now().UTC())
	if s.deps.Events != nil {
		s.deps.Events.Emit(dto.EventProjectStarted, projectID, "",
			registry.ProjectDisplayName(record, cfg)+" started", nil)
	}
	return result, nil
}

// StopProject stops the running services of a project, dependents first.
func (s *Supervisor) StopProject(projectID string, names []string, all bool) (dto.OperationResult, error) {
	cfg, err := s.deps.Registry.Config(projectID)
	if err != nil {
		return dto.OperationResult{}, err
	}
	selected, err := cfg.ResolveServiceSet(names, "", all || len(names) == 0)
	if err != nil {
		return dto.OperationResult{}, err
	}
	ordered, err := cfg.TopoOrder(selected)
	if err != nil {
		// A cycle must not prevent a stop; fall back to the declared order.
		ordered = selected
	}

	result := dto.OperationResult{Project: projectID}
	// Stopping happens in reverse dependency order so a database outlives the
	// services that talk to it.
	for i := len(ordered) - 1; i >= 0; i-- {
		name := ordered[i]
		spec, _ := cfg.Service(name)
		sv := s.serviceState(projectID, name)

		sv.opMu.Lock()
		stopErr := sv.stopInstance(s.gracefulTimeout(spec))
		sv.opMu.Unlock()
		if stopErr != nil {
			result.Errors = append(result.Errors, *dto.FromError(stopErr))
		}
		result.Services = append(result.Services, s.serviceDTO(sv, spec))
	}
	if s.deps.Events != nil {
		s.deps.Events.Emit(dto.EventProjectStopped, projectID, "", "project stopped", nil)
	}
	return result, nil
}

// awaitDependencies blocks until every declared dependency satisfies its
// condition, or reports DEPENDENCY_FAILED.
func (s *Supervisor) awaitDependencies(projectID string, spec *config.Service) error {
	if len(spec.DependsOn) == 0 {
		return nil
	}
	timeout := s.deps.Settings().Defaults.StartTimeout.Duration
	if timeout <= 0 {
		timeout = 60 * time.Second
	}

	for _, dep := range spec.DependsOn {
		depSv, tracked := s.known(projectID, dep.Name)
		if !tracked || !depSv.running() {
			return errs.New(errs.CodeDependencyFailed,
				"%s depends on %s, which is not running", spec.Name, dep.Name).
				With("dependency", dep.Name)
		}
		if dep.Condition != config.ConditionHealthy {
			continue
		}
		depSv.mu.Lock()
		monitor := depSv.monitor
		depSv.mu.Unlock()
		if monitor == nil {
			return errs.New(errs.CodeDependencyFailed,
				"%s waits for %s to be healthy, but it is not being monitored",
				spec.Name, dep.Name).With("dependency", dep.Name)
		}
		if _, err := monitor.WaitHealthy(s.ctx, timeout); err != nil {
			return errs.New(errs.CodeDependencyFailed,
				"%s waited %s for %s to become healthy", spec.Name, timeout, dep.Name).
				With("dependency", dep.Name)
		}
	}
	return nil
}

// serviceDTO renders the API view of a service.
func (s *Supervisor) serviceDTO(sv *service, spec *config.Service) dto.Service {
	snapshot := sv.snapshot()
	out := dto.Service{
		Project:      sv.projectID,
		Name:         sv.name,
		Status:       snapshot.actual,
		DesiredState: snapshot.desired,
		Health:       sv.healthResult(),
		RestartCount: snapshot.restarts,
		LastExitCode: snapshot.lastExit,
		CommandLine:  snapshot.commandLine,
		CWD:          snapshot.cwd,
		Ports:        snapshot.ports,
		Message:      snapshot.message,
		Observability: dto.Observability{
			LogCapture: snapshot.logCapture,
			Adopted:    snapshot.adopted,
		},
	}
	if snapshot.reason != nil {
		out.Reason = dto.FromError(snapshot.reason)
	}
	if spec != nil {
		out.DisplayName = spec.DisplayName
		out.Runtime = string(spec.Runtime)
		for _, dep := range spec.DependsOn {
			out.DependsOn = append(out.DependsOn, dep.Name)
		}
	} else {
		out.Runtime = string(snapshot.kind)
	}

	sv.mu.Lock()
	handle := sv.handle
	sv.mu.Unlock()
	if handle != nil {
		out.PID = handle.PID()
	}
	if !snapshot.startedAt.IsZero() && snapshot.actual == dto.StatusRunning {
		started := snapshot.startedAt
		out.StartedAt = &started
		out.UptimeSeconds = int64(time.Since(started).Seconds())
	}
	out.URL = serviceURL(spec, snapshot.ports)
	return out
}

// serviceURL builds the browsable address of a service, preferring the port
// named "http".
func serviceURL(spec *config.Service, ports []dto.PortAllocation) string {
	if len(ports) == 0 {
		return ""
	}
	preferred := "http"
	if spec != nil {
		if name, ok := spec.PrimaryPortName(); ok {
			preferred = name
		}
	}
	for _, allocation := range ports {
		if allocation.Name == preferred {
			return fmt.Sprintf("http://127.0.0.1:%d", allocation.Port)
		}
	}
	return ""
}

// ServiceStatus returns the current view of one service.
func (s *Supervisor) ServiceStatus(projectID, name string) (dto.Service, error) {
	cfg, err := s.deps.Registry.Config(projectID)
	var spec *config.Service
	if err == nil {
		spec, _ = cfg.Service(name)
	}
	sv, tracked := s.known(projectID, name)
	if !tracked {
		if spec == nil {
			return dto.Service{}, errs.New(errs.CodeServiceNotFound,
				"project %s has no service %s", projectID, name)
		}
		sv = s.serviceState(projectID, name)
	}
	return s.serviceDTO(sv, spec), nil
}

// ProjectServices returns every declared service of a project with its state.
func (s *Supervisor) ProjectServices(projectID string, cfg *config.Config) []dto.Service {
	out := make([]dto.Service, 0, len(cfg.Services))
	for _, name := range cfg.ServiceNames() {
		spec, err := cfg.Service(name)
		if err != nil {
			continue
		}
		sv := s.serviceState(projectID, name)
		out = append(out, s.serviceDTO(sv, spec))
	}
	return out
}

// Summarise aggregates service states into the project headline.
//
// Services the user has not asked to run are left out of the verdict. A project
// with an optional worker, or one service deliberately stopped, is not degraded:
// it is in the state that was asked for. Total still counts every declared
// service so a caller can show "1/2 running" and see that something is idle.
func Summarise(services []dto.Service) (dto.ProjectSummary, dto.ProjectStatus) {
	summary := dto.ProjectSummary{Total: len(services)}
	wanted := 0
	status := dto.ProjectStopped
	for _, svc := range services {
		if svc.DesiredState == dto.DesiredStopped {
			continue
		}
		wanted++
		switch svc.Status {
		case dto.StatusRunning:
			summary.Running++
		case dto.StatusFailed, dto.StatusCrashed, dto.StatusBlocked:
			summary.Failed++
		}
		if svc.Health.Status == dto.HealthHealthy || svc.Health.Status == dto.HealthNotApplicable {
			if svc.Status == dto.StatusRunning {
				summary.Healthy++
			}
		}
	}
	switch {
	case summary.Total == 0:
		status = dto.ProjectStopped
	case wanted == 0:
		status = dto.ProjectStopped
	case summary.Failed > 0 && summary.Running == 0:
		status = dto.ProjectFailed
	case summary.Failed > 0:
		status = dto.ProjectDegraded
	case summary.Running == 0:
		status = dto.ProjectStopped
	case summary.Healthy == summary.Running && summary.Running == wanted:
		status = dto.ProjectHealthy
	case summary.Running > 0:
		status = dto.ProjectDegraded
	}
	return summary, status
}
