package supervisor

import (
	"os"
	"strconv"
	"time"

	"github.com/devman-project/devman/internal/envresolve"
	"github.com/devman-project/devman/internal/health"
	"github.com/devman-project/devman/internal/portmgr"
	"github.com/devman-project/devman/internal/settings"
	"github.com/devman-project/devman/pkg/config"
	"github.com/devman-project/devman/pkg/errs"
)

// launchPlan is everything needed to start one service, fully resolved.
//
// It is computed in a single place so a failure to resolve anything (a missing
// variable, an occupied port, an executable that is not installed) is reported
// before a process is created, rather than half-way through a start.
type launchPlan struct {
	Kind    config.RuntimeKind
	Command string
	Args    []string
	Shell   config.ShellSpec
	Dir     string
	Env     []string
	Compose *config.ComposeSpec

	Ports      portmgr.Allocation
	Health     health.Spec
	Graceful   time.Duration
	StartWait  time.Duration
	Restart    restartPolicy
	Injected   map[string]string
	CommandRaw string
}

// restartPolicy is the resolved restart configuration.
type restartPolicy struct {
	Policy      config.RestartPolicy
	MaxAttempts int
	Delay       time.Duration
	MaxDelay    time.Duration
}

// prepare resolves a service into a launch plan, reserving its ports.
//
// Ports are reserved here, before the process exists, because a reservation is
// what makes PORT injection meaningful: the value handed to the service is
// already exclusively ours.
func (sv *service) prepare(cfg *config.Config, spec *config.Service) (launchPlan, error) {
	current := sv.sup.deps.Settings()
	platformKey := config.CurrentPlatform()
	execution := spec.Execution(platformKey)
	dir := spec.AbsCWD(cfg.ProjectRoot, platformKey)

	plan := launchPlan{
		Kind:      spec.Runtime,
		Shell:     execution.Shell,
		Dir:       dir,
		Compose:   spec.Compose,
		Graceful:  spec.GracefulTimeout.Or(current.Defaults.GracefulTimeout.Duration),
		StartWait: current.Defaults.StartTimeout.Duration,
		Restart:   resolveRestart(spec.Restart, current),
	}

	// Layer 1 and 2: the daemon environment and the declared env files.
	base := envresolve.CurrentEnv()
	resolver := envresolve.Resolver{
		AdditionalPath: current.Environment.AdditionalPath,
		ExtraEnv:       current.Environment.Env,
	}
	resolver.ApplyPath(base)
	files, err := envresolve.LoadFiles(cfg.ProjectRoot, spec.EnvFile)
	if err != nil {
		return launchPlan{}, err
	}

	home, _ := os.UserHomeDir()

	// Service env values may themselves use templates, so they are expanded
	// against the layers below them. That keeps the precedence chain acyclic:
	// a service variable can read the daemon environment, never itself.
	lower := envresolve.Layers{Base: base, Files: files}.UserEnv()
	lowerContext := config.TemplateContext{
		ProjectDir: cfg.ProjectRoot,
		ServiceDir: dir,
		Home:       home,
		Env: func(name string) (string, bool) {
			value, ok := lower[name]
			return value, ok
		},
	}
	serviceEnv := make(map[string]string, len(execution.Env))
	for key, value := range execution.Env {
		expanded, expandErr := lowerContext.Expand(value)
		if expandErr != nil {
			return launchPlan{}, errs.From(expandErr).At("services." + spec.Name + ".env." + key)
		}
		serviceEnv[key] = expanded
	}

	layers := envresolve.Layers{Base: base, Files: files, Service: serviceEnv}
	userEnv := layers.UserEnv()

	// required_env is checked against the user layers only: a variable DevMan
	// would inject itself is not something the project has to provide.
	if missing := envresolve.MissingRequired(spec.RequiredEnv, userEnv); len(missing) > 0 {
		return launchPlan{}, errs.New(errs.CodeServiceBlocked,
			"required environment variables are not set: %v", missing).
			At("services."+spec.Name+".required_env").
			With("missing", missing)
	}

	// Ports are reserved through the port manager, the only component allowed
	// to hand out a port number.
	//
	// An external service is the exception: it is already listening, so its
	// ports are adopted instead of reserved. Reserving would fail the OS probe
	// and block a service that is, in fact, perfectly healthy.
	var allocation portmgr.Allocation
	if spec.Runtime == config.RuntimeExternal {
		allocation, err = sv.adoptDeclaredPorts(spec)
	} else {
		allocation, err = sv.sup.deps.Ports.ReserveService(sv.projectID, sv.name, spec.Ports)
	}
	if err != nil {
		return launchPlan{}, err
	}
	plan.Ports = allocation

	releasePorts := func() {
		_ = sv.sup.deps.Ports.ReleaseService(sv.projectID, sv.name)
	}

	// Layer 5: DevMan's own injection, which deliberately wins over .env. A
	// declared `env: PORT` is a statement that DevMan owns the variable.
	injected := map[string]string{
		"DEVMAN_PROJECT_ID":   sv.projectID,
		"DEVMAN_PROJECT_NAME": cfg.Project.Name,
		"DEVMAN_SERVICE_NAME": sv.name,
		"DEVMAN_PROJECT_DIR":  cfg.ProjectRoot,
	}
	for key, value := range allocation.Env {
		injected[key] = value
	}
	layers.Injection = injected
	plan.Injected = injected
	plan.Env = envresolve.Environ(layers.Final())

	defaultPort, _ := spec.PrimaryPortName()
	expandContext := config.TemplateContext{
		ProjectDir:      cfg.ProjectRoot,
		ServiceDir:      dir,
		Home:            home,
		Ports:           allocation.ByName,
		DefaultPortName: defaultPort,
		Env: func(name string) (string, bool) {
			value, ok := userEnv[name]
			return value, ok
		},
	}

	command, err := expandContext.Expand(execution.Command)
	if err != nil {
		releasePorts()
		return launchPlan{}, errs.From(err).At("services." + spec.Name + ".command")
	}
	args, err := expandContext.ExpandAll(execution.Args)
	if err != nil {
		releasePorts()
		return launchPlan{}, errs.From(err).At("services." + spec.Name + ".args")
	}
	plan.CommandRaw = command
	plan.Args = args

	// A host command is resolved against the daemon PATH plus any configured
	// additional directories. The absolute path is runtime state and is never
	// written back into devman.yaml, which has to stay portable.
	plan.Command = command
	if spec.Runtime == config.RuntimeHost && !execution.Shell.Enabled {
		resolved, lookErr := resolver.Lookup(command, dir, resolver.PathValue(base))
		if lookErr != nil {
			releasePorts()
			return launchPlan{}, errs.From(lookErr).At("services." + spec.Name + ".command")
		}
		plan.Command = resolved
	}

	healthSpec, err := health.SpecFrom(spec.Health, expandContext.Expand, health.Defaults{
		Interval: current.Defaults.HealthInterval.Duration,
		Timeout:  current.Defaults.HealthTimeout.Duration,
		Retries:  current.Defaults.HealthRetries,
	})
	if err != nil {
		releasePorts()
		return launchPlan{}, errs.From(err).At("services." + spec.Name + ".health")
	}
	plan.Health = healthSpec

	return plan, nil
}

// adoptDeclaredPorts records the ports of an external service without probing
// them for availability: the point of an external service is that something
// else is already listening.
func (sv *service) adoptDeclaredPorts(spec *config.Service) (portmgr.Allocation, error) {
	allocation := portmgr.Allocation{
		ByName: map[string]int{},
		Env:    map[string]string{},
	}
	for _, port := range spec.Ports {
		if port.Value.Auto {
			return portmgr.Allocation{}, errs.New(errs.CodeConfigInvalid,
				"an external service cannot use `value: auto`; declare the port it actually listens on").
				At("services." + spec.Name + ".ports." + port.Name)
		}
		record, err := sv.sup.deps.Ports.Adopt(
			sv.projectID, sv.name, port.Name, port.Env, port.Value.Number)
		if err != nil {
			return portmgr.Allocation{}, err
		}
		allocation.Records = append(allocation.Records, record)
		allocation.ByName[port.Name] = record.Port
		if port.Env != "" {
			allocation.Env[port.Env] = strconv.Itoa(record.Port)
		}
	}
	return allocation, nil
}

// resolveRestart merges the declared restart block with the global defaults.
func resolveRestart(spec *config.RestartSpec, current *settings.Settings) restartPolicy {
	policy := restartPolicy{
		Policy:   config.RestartNo,
		Delay:    current.Defaults.RestartDelay.Duration,
		MaxDelay: current.Defaults.RestartMaxDelay.Duration,
	}
	if spec == nil {
		return policy
	}
	policy.Policy = spec.Policy
	policy.MaxAttempts = spec.MaxAttempts
	if spec.Delay != nil && spec.Delay.Duration > 0 {
		policy.Delay = spec.Delay.Duration
	}
	if spec.MaxDelay != nil && spec.MaxDelay.Duration > 0 {
		policy.MaxDelay = spec.MaxDelay.Duration
	}
	return policy
}
