package config

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/devman-project/devman/pkg/errs"
)

var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
var envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidateOptions controls how much of the host environment validation is
// allowed to touch. `devman validate` enables everything; parsing a config for
// presentation may disable filesystem and PATH probing.
type ValidateOptions struct {
	// CheckFilesystem verifies cwd, env_file and compose file existence.
	CheckFilesystem bool
	// CheckCommands resolves `command` against PATH.
	CheckCommands bool
	// Platform is the overlay key used for command resolution.
	Platform string
}

// DefaultValidateOptions is the behaviour of `devman validate`.
func DefaultValidateOptions() ValidateOptions {
	return ValidateOptions{CheckFilesystem: true, CheckCommands: true, Platform: CurrentPlatform()}
}

// ValidationResult is the machine readable output of `devman validate --json`.
type ValidationResult struct {
	Valid    bool          `json:"valid"`
	Errors   []*errs.Error `json:"errors"`
	Warnings []*errs.Error `json:"warnings"`
}

func (r *ValidationResult) addError(err *errs.Error)   { r.Errors = append(r.Errors, err) }
func (r *ValidationResult) addWarning(err *errs.Error) { r.Warnings = append(r.Warnings, err) }

// Err returns the first error as a Go error, or nil when the config is valid.
func (r *ValidationResult) Err() error {
	if r.Valid {
		return nil
	}
	if len(r.Errors) == 0 {
		return errs.New(errs.CodeConfigInvalid, "configuration is invalid")
	}
	first := r.Errors[0]
	if len(r.Errors) == 1 {
		return first
	}
	return first.With("additional_errors", len(r.Errors)-1)
}

// Validate performs the full V0.1 validation pass.
func (c *Config) Validate(opts ValidateOptions) *ValidationResult {
	if opts.Platform == "" {
		opts.Platform = CurrentPlatform()
	}
	result := &ValidationResult{Errors: []*errs.Error{}, Warnings: []*errs.Error{}}

	if c.Version != SchemaVersion {
		result.addError(errs.New(errs.CodeConfigInvalid,
			"unsupported version %d, expected %d", c.Version, SchemaVersion).At("version"))
	}
	if strings.TrimSpace(c.Project.Name) == "" {
		result.addError(errs.New(errs.CodeConfigInvalid, "project name is required").At("project.name"))
	} else if !nameRe.MatchString(c.Project.Name) {
		result.addError(errs.New(errs.CodeConfigInvalid,
			"project name %q must match %s", c.Project.Name, nameRe.String()).At("project.name"))
	}
	if len(c.Services) == 0 {
		result.addError(errs.New(errs.CodeConfigInvalid, "at least one service is required").At("services"))
		result.Valid = len(result.Errors) == 0
		return result
	}

	fixedPorts := map[int]string{}
	for _, name := range c.ServiceNames() {
		svc := c.Services[name]
		path := "services." + name
		if !nameRe.MatchString(name) {
			result.addError(errs.New(errs.CodeConfigInvalid,
				"service name %q must match %s", name, nameRe.String()).At(path))
		}
		c.validateService(svc, path, opts, result, fixedPorts)
	}

	c.validateDependencies(result)
	c.validateServiceSets(result)

	result.Valid = len(result.Errors) == 0
	return result
}

func (c *Config) validateService(
	svc *Service,
	path string,
	opts ValidateOptions,
	result *ValidationResult,
	fixedPorts map[int]string,
) {
	switch svc.Runtime {
	case RuntimeHost:
		if strings.TrimSpace(svc.Command) == "" {
			result.addError(errs.New(errs.CodeConfigInvalid,
				"command is required for runtime host").At(path + ".command"))
		}
	case RuntimeExternal:
		if svc.Command != "" || len(svc.Args) > 0 {
			result.addError(errs.New(errs.CodeConfigInvalid,
				"runtime external is monitor-only and must not define command or args").At(path + ".command"))
		}
	case RuntimeDockerCompose:
		if svc.Command != "" || len(svc.Args) > 0 {
			result.addError(errs.New(errs.CodeConfigInvalid,
				"runtime docker-compose must not define command or args; use compose.service").At(path + ".command"))
		}
		if svc.Compose == nil || strings.TrimSpace(svc.Compose.Service) == "" {
			result.addError(errs.New(errs.CodeConfigInvalid,
				"compose.service is required for runtime docker-compose").At(path + ".compose.service"))
		}
	default:
		result.addError(errs.New(errs.CodeConfigInvalid,
			"unknown runtime %q (expected host, docker-compose or external)", svc.Runtime).At(path + ".runtime"))
	}

	if svc.Shell.Enabled && len(svc.Args) > 0 {
		result.addError(errs.New(errs.CodeConfigInvalid,
			"shell: true requires a single command string; args must be empty").At(path + ".args"))
	}
	if svc.Shell.Type != ShellDefault && svc.Shell.Type != ShellPowerShell {
		result.addError(errs.New(errs.CodeConfigInvalid,
			"unknown shell type %q (expected powershell)", svc.Shell.Type).At(path + ".shell.type"))
	}
	if svc.GracefulTimeout != nil && svc.GracefulTimeout.Duration <= 0 {
		result.addError(errs.New(errs.CodeConfigInvalid,
			"graceful_timeout must be positive").At(path + ".graceful_timeout"))
	}

	// Ports.
	portNames := make([]string, 0, len(svc.Ports))
	seenPortNames := map[string]bool{}
	for i, port := range svc.Ports {
		portPath := fmt.Sprintf("%s.ports[%d]", path, i)
		if strings.TrimSpace(port.Name) == "" {
			result.addError(errs.New(errs.CodeConfigInvalid, "port name is required").At(portPath + ".name"))
		} else if !nameRe.MatchString(port.Name) {
			result.addError(errs.New(errs.CodeConfigInvalid,
				"port name %q must match %s", port.Name, nameRe.String()).At(portPath + ".name"))
		} else if seenPortNames[port.Name] {
			result.addError(errs.New(errs.CodeConfigInvalid,
				"duplicate port name %q", port.Name).At(portPath + ".name"))
		} else {
			seenPortNames[port.Name] = true
			portNames = append(portNames, port.Name)
		}
		if !port.Value.Auto {
			if port.Value.Number < 1 || port.Value.Number > 65535 {
				result.addError(errs.New(errs.CodeConfigInvalid,
					"port %d is out of range 1-65535", port.Value.Number).At(portPath + ".value"))
			} else if owner, clash := fixedPorts[port.Value.Number]; clash {
				result.addError(errs.New(errs.CodePortConflict,
					"fixed port %d is already claimed by %s", port.Value.Number, owner).At(portPath + ".value"))
			} else {
				fixedPorts[port.Value.Number] = svc.Name + "." + port.Name
			}
			if port.Preferred != 0 {
				result.addError(errs.New(errs.CodeConfigInvalid,
					"preferred only applies when value is auto").At(portPath + ".preferred"))
			}
		} else if port.Preferred != 0 && (port.Preferred < 1 || port.Preferred > 65535) {
			result.addError(errs.New(errs.CodeConfigInvalid,
				"preferred port %d is out of range 1-65535", port.Preferred).At(portPath + ".preferred"))
		}
		if port.Env != "" && !envNameRe.MatchString(port.Env) {
			result.addError(errs.New(errs.CodeConfigInvalid,
				"%q is not a valid environment variable name", port.Env).At(portPath + ".env"))
		}
	}
	_, hasDefaultPort := svc.PrimaryPortName()

	// Templates.
	exec := svc.Execution(opts.Platform)
	c.checkTemplate(result, path+".command", exec.Command, portNames, hasDefaultPort)
	for i, arg := range exec.Args {
		c.checkTemplate(result, fmt.Sprintf("%s.args[%d]", path, i), arg, portNames, hasDefaultPort)
	}
	c.checkTemplate(result, path+".cwd", exec.CWD, portNames, hasDefaultPort)
	for _, key := range sortedKeys(exec.Env) {
		c.checkTemplate(result, path+".env."+key, exec.Env[key], portNames, hasDefaultPort)
		if !envNameRe.MatchString(key) {
			result.addError(errs.New(errs.CodeConfigInvalid,
				"%q is not a valid environment variable name", key).At(path + ".env"))
		}
	}
	for _, key := range svc.RequiredEnv {
		if !envNameRe.MatchString(key) {
			result.addError(errs.New(errs.CodeConfigInvalid,
				"%q is not a valid environment variable name", key).At(path + ".required_env"))
		}
	}

	// Health.
	if svc.Health != nil {
		hp := path + ".health"
		switch svc.Health.Type {
		case HealthProcess:
			if svc.Runtime == RuntimeExternal {
				result.addError(errs.New(errs.CodeConfigInvalid,
					"runtime external cannot use process health; use tcp or http").At(hp + ".type"))
			}
		case HealthTCP:
			if svc.Health.Port == "" {
				result.addError(errs.New(errs.CodeConfigInvalid,
					"tcp health requires a port").At(hp + ".port"))
			}
			c.checkTemplate(result, hp+".port", svc.Health.Port, portNames, hasDefaultPort)
			c.checkTemplate(result, hp+".host", svc.Health.Host, portNames, hasDefaultPort)
		case HealthHTTP:
			if strings.TrimSpace(svc.Health.URL) == "" {
				result.addError(errs.New(errs.CodeConfigInvalid,
					"http health requires a url").At(hp + ".url"))
			}
			c.checkTemplate(result, hp+".url", svc.Health.URL, portNames, hasDefaultPort)
			for _, status := range svc.Health.ExpectedStatus {
				if status < 100 || status > 599 {
					result.addError(errs.New(errs.CodeConfigInvalid,
						"expected_status %d is not a valid HTTP status", status).At(hp + ".expected_status"))
				}
			}
		default:
			result.addError(errs.New(errs.CodeConfigInvalid,
				"unknown health type %q (expected process, tcp or http)", svc.Health.Type).At(hp + ".type"))
		}
		if svc.Health.Interval != nil && svc.Health.Interval.Duration <= 0 {
			result.addError(errs.New(errs.CodeConfigInvalid, "interval must be positive").At(hp + ".interval"))
		}
		if svc.Health.Timeout != nil && svc.Health.Timeout.Duration <= 0 {
			result.addError(errs.New(errs.CodeConfigInvalid, "timeout must be positive").At(hp + ".timeout"))
		}
		if svc.Health.Retries < 0 {
			result.addError(errs.New(errs.CodeConfigInvalid, "retries must not be negative").At(hp + ".retries"))
		}
	}

	// Restart.
	if svc.Restart != nil {
		rp := path + ".restart"
		switch svc.Restart.Policy {
		case RestartNo, RestartOnFailure, RestartAlways:
		default:
			result.addError(errs.New(errs.CodeConfigInvalid,
				"unknown restart policy %q (expected no, on-failure or always)", svc.Restart.Policy).At(rp + ".policy"))
		}
		if svc.Restart.MaxAttempts < 0 {
			result.addError(errs.New(errs.CodeConfigInvalid,
				"max_attempts must not be negative").At(rp + ".max_attempts"))
		}
		if svc.Restart.Delay != nil && svc.Restart.Delay.Duration <= 0 {
			result.addError(errs.New(errs.CodeConfigInvalid, "delay must be positive").At(rp + ".delay"))
		}
		if svc.Restart.MaxDelay != nil && svc.Restart.Delay != nil &&
			svc.Restart.MaxDelay.Duration < svc.Restart.Delay.Duration {
			result.addError(errs.New(errs.CodeConfigInvalid,
				"max_delay must not be smaller than delay").At(rp + ".max_delay"))
		}
		if svc.Restart.Policy != RestartNo && svc.Runtime == RuntimeExternal {
			result.addError(errs.New(errs.CodeConfigInvalid,
				"runtime external cannot define a restart policy").At(rp + ".policy"))
		}
	}

	// Paths are checked against the platform overlay so `platform.windows.cwd`
	// is honoured on Windows.
	if !opts.CheckFilesystem || c.ProjectRoot == "" {
		return
	}
	cwd := svc.AbsCWD(c.ProjectRoot, opts.Platform)
	if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
		result.addError(errs.New(errs.CodeConfigInvalid,
			"directory does not exist: %s", cwd).At(path + ".cwd"))
	}
	for i, envFile := range svc.EnvFile {
		full := envFile
		if !filepath.IsAbs(full) {
			full = filepath.Join(cwd, envFile)
		}
		if _, err := os.Stat(full); err != nil {
			// Missing .env.local is normal, so this is not fatal.
			result.addWarning(errs.New(errs.CodeConfigInvalid,
				"env_file not found: %s", full).At(fmt.Sprintf("%s.env_file[%d]", path, i)))
		}
	}
	if svc.Runtime == RuntimeDockerCompose && svc.Compose != nil && svc.Compose.File != "" {
		// Relative to the service's cwd, because that is where the runtime runs
		// `docker compose -f`. Resolving it against the project root here would
		// let validation pass on a file the runtime never opens.
		full := svc.Compose.File
		if !filepath.IsAbs(full) {
			full = filepath.Join(cwd, full)
		}
		if _, err := os.Stat(full); err != nil {
			result.addError(errs.New(errs.CodeConfigInvalid,
				"compose file not found: %s", full).At(path + ".compose.file"))
		} else {
			c.checkComposeService(result, path, full, svc)
		}
	}
	if opts.CheckCommands && svc.Runtime == RuntimeHost && exec.Command != "" && !svc.Shell.Enabled {
		if _, err := lookPath(exec.Command, cwd); err != nil {
			result.addWarning(errs.New(errs.CodeCommandNotFound,
				"command %q was not found on PATH", exec.Command).At(path + ".command"))
		}
	}
}

// checkComposeService verifies that the compose file really declares the service
// the configuration points at.
//
// Without this, a typo in `compose.service` is only discovered when someone
// presses start: `docker compose up` fails, and a name that could have been
// checked in milliseconds becomes a runtime failure. `devman validate` exists to
// answer "will this work", so it has to read the file it delegates to.
//
// The check is deliberately conservative. Compose files can assemble their
// services from other files through `include` or `extends`, and DevMan does not
// reimplement that resolution; when one of those is present the service list is
// incomplete and no conclusion is drawn. Silence is better than a false error
// about a service that does exist.
func (c *Config) checkComposeService(result *ValidationResult, path, file string, svc *Service) {
	declared := strings.TrimSpace(svc.Compose.Service)
	if declared == "" {
		return
	}

	body, err := os.ReadFile(file)
	if err != nil {
		return
	}

	var document struct {
		Services map[string]yaml.Node `yaml:"services"`
		Include  yaml.Node            `yaml:"include"`
	}
	if err := yaml.Unmarshal(body, &document); err != nil {
		result.addWarning(errs.New(errs.CodeConfigInvalid,
			"compose file %s could not be parsed, so its services were not checked: %v", file, err).
			At(path + ".compose.file"))
		return
	}
	if !document.Include.IsZero() || len(document.Services) == 0 {
		return
	}
	if _, ok := document.Services[declared]; ok {
		return
	}
	for _, node := range document.Services {
		// `extends` pulls a definition in from elsewhere, so the file may be an
		// incomplete picture of what compose will end up knowing.
		var candidate struct {
			Extends yaml.Node `yaml:"extends"`
		}
		if err := node.Decode(&candidate); err == nil && !candidate.Extends.IsZero() {
			return
		}
	}

	names := make([]string, 0, len(document.Services))
	for name := range document.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	result.addError(errs.New(errs.CodeConfigInvalid,
		"compose file %s has no service %q; it declares %s",
		file, declared, strings.Join(names, ", ")).At(path + ".compose.service"))
}

// lookPath resolves a command, also accepting a path relative to cwd.
func lookPath(command, cwd string) (string, error) {
	if strings.ContainsAny(command, `/\`) {
		candidate := command
		if !filepath.IsAbs(candidate) {
			candidate = filepath.Join(cwd, candidate)
		}
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return exec.LookPath(command)
}

func (c *Config) checkTemplate(
	result *ValidationResult,
	path, value string,
	portNames []string,
	hasDefaultPort bool,
) {
	if value == "" {
		return
	}
	if err := CheckTemplateSyntax(value, portNames, hasDefaultPort); err != nil {
		result.addError(errs.From(err).At(path))
	}
}

func (c *Config) validateDependencies(result *ValidationResult) {
	for _, name := range c.ServiceNames() {
		svc := c.Services[name]
		seen := map[string]bool{}
		for i, dep := range svc.DependsOn {
			depPath := fmt.Sprintf("services.%s.depends_on[%d]", name, i)
			if dep.Name == name {
				result.addError(errs.New(errs.CodeConfigInvalid,
					"service %q cannot depend on itself", name).At(depPath))
				continue
			}
			if _, ok := c.Services[dep.Name]; !ok {
				result.addError(errs.New(errs.CodeConfigInvalid,
					"depends_on refers to unknown service %q", dep.Name).At(depPath))
				continue
			}
			if seen[dep.Name] {
				result.addError(errs.New(errs.CodeConfigInvalid,
					"duplicate dependency %q", dep.Name).At(depPath))
			}
			seen[dep.Name] = true
			switch dep.Condition {
			case ConditionStarted, ConditionHealthy:
			default:
				result.addError(errs.New(errs.CodeConfigInvalid,
					"unknown condition %q (expected started or healthy)", dep.Condition).At(depPath + ".condition"))
			}
			if dep.Condition == ConditionHealthy {
				target := c.Services[dep.Name]
				if target.Runtime == RuntimeExternal && target.Health != nil &&
					target.Health.Type == HealthProcess {
					result.addError(errs.New(errs.CodeConfigInvalid,
						"condition healthy requires %q to define a tcp or http health check", dep.Name).At(depPath))
				}
			}
		}
	}
	if cycle := c.findCycle(); len(cycle) > 0 {
		result.addError(errs.New(errs.CodeConfigInvalid,
			"dependency cycle: %s", strings.Join(cycle, " -> ")).At("services"))
	}
}

// findCycle returns one dependency cycle, or nil.
func (c *Config) findCycle() []string {
	const (
		white = 0
		grey  = 1
		black = 2
	)
	color := map[string]int{}
	var stack []string
	var cycle []string

	var visit func(string) bool
	visit = func(name string) bool {
		color[name] = grey
		stack = append(stack, name)
		svc := c.Services[name]
		if svc != nil {
			for _, dep := range svc.DependsOn {
				if _, ok := c.Services[dep.Name]; !ok || dep.Name == name {
					continue
				}
				switch color[dep.Name] {
				case white:
					if visit(dep.Name) {
						return true
					}
				case grey:
					start := 0
					for i, item := range stack {
						if item == dep.Name {
							start = i
							break
						}
					}
					cycle = append(append([]string(nil), stack[start:]...), dep.Name)
					return true
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[name] = black
		return false
	}

	for _, name := range c.ServiceNames() {
		if color[name] == white && visit(name) {
			return cycle
		}
	}
	return nil
}

func (c *Config) validateServiceSets(result *ValidationResult) {
	if c.Startup != nil {
		for i, name := range c.Startup.Default {
			if _, ok := c.Services[name]; !ok {
				result.addError(errs.New(errs.CodeConfigInvalid,
					"startup.default refers to unknown service %q", name).
					At(fmt.Sprintf("startup.default[%d]", i)))
			}
		}
	}
	for _, profile := range sortedKeys(c.Profiles) {
		if len(c.Profiles[profile]) == 0 {
			result.addError(errs.New(errs.CodeConfigInvalid,
				"profile %q is empty", profile).At("profiles." + profile))
		}
		for i, name := range c.Profiles[profile] {
			if _, ok := c.Services[name]; !ok {
				result.addError(errs.New(errs.CodeConfigInvalid,
					"profile %q refers to unknown service %q", profile, name).
					At(fmt.Sprintf("profiles.%s[%d]", profile, i)))
			}
		}
	}
}

// TopoOrder returns a start order for the given services, expanded with their
// transitive dependencies and topologically sorted.
func (c *Config) TopoOrder(names []string) ([]string, error) {
	if cycle := c.findCycle(); len(cycle) > 0 {
		return nil, errs.New(errs.CodeConfigInvalid, "dependency cycle: %s", strings.Join(cycle, " -> "))
	}
	visited := map[string]bool{}
	var order []string
	var visit func(string) error
	visit = func(name string) error {
		if visited[name] {
			return nil
		}
		svc, err := c.Service(name)
		if err != nil {
			return err
		}
		visited[name] = true
		for _, dep := range svc.DependsOn {
			if err := visit(dep.Name); err != nil {
				return err
			}
		}
		order = append(order, name)
		return nil
	}
	for _, name := range names {
		if err := visit(name); err != nil {
			return nil, err
		}
	}
	return order, nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
