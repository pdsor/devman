package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/devman-project/devman/pkg/errs"
	"gopkg.in/yaml.v3"
)

// CanonicalFileName is where `devman init` and the Codex skill always write.
const CanonicalFileName = "devman.yaml"

// discoveryOrder lists accepted config locations relative to the project root,
// highest precedence first.
var discoveryOrder = []string{
	"devman.yaml",
	"devman.yml",
	filepath.Join(".devman", "devman.yaml"),
	filepath.Join(".devman", "devman.yml"),
}

// Discover returns the config path for a project root, following the fixed
// precedence order. It returns CodeConfigNotFound when nothing is present.
func Discover(root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", errs.Wrap(errs.CodeInternal, err, "cannot resolve %q", root)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", errs.New(errs.CodeConfigNotFound, "path does not exist: %s", abs)
	}
	if !info.IsDir() {
		// A direct file reference is accepted as-is.
		return abs, nil
	}
	for _, candidate := range discoveryOrder {
		full := filepath.Join(abs, candidate)
		if st, statErr := os.Stat(full); statErr == nil && !st.IsDir() {
			return full, nil
		}
	}
	return "", errs.New(errs.CodeConfigNotFound,
		"no devman.yaml found in %s (run `devman init`)", abs)
}

// Load discovers, parses and normalises the configuration for a project root
// (or an explicit config file path).
func Load(pathOrRoot string) (*Config, error) {
	configPath, err := Discover(pathOrRoot)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, errs.Wrap(errs.CodeConfigInvalid, err, "cannot read %s", configPath)
	}
	cfg, err := Parse(data)
	if err != nil {
		return nil, err
	}
	cfg.ConfigPath = configPath
	cfg.ProjectRoot = projectRootFor(configPath)
	return cfg, nil
}

// projectRootFor maps a config file path back to the project root, accounting
// for the `.devman/` location.
func projectRootFor(configPath string) string {
	dir := filepath.Dir(configPath)
	if strings.EqualFold(filepath.Base(dir), ".devman") {
		return filepath.Dir(dir)
	}
	return dir
}

// Parse decodes and normalises YAML bytes. Unknown fields are rejected so
// typos surface immediately instead of being silently ignored.
func Parse(data []byte) (*Config, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, errs.Wrap(errs.CodeConfigInvalid, err, "invalid YAML")
	}
	if root.Kind == 0 || len(root.Content) == 0 {
		return nil, errs.New(errs.CodeConfigInvalid, "configuration is empty")
	}

	cfg := &Config{}
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)
	if err := decoder.Decode(cfg); err != nil && !errors.Is(err, os.ErrDeadlineExceeded) {
		return nil, errs.Wrap(errs.CodeConfigInvalid, err, "invalid configuration")
	}

	cfg.ServiceOrder = serviceOrderFromNode(root.Content[0], cfg.Services)
	cfg.normalize()
	return cfg, nil
}

// serviceOrderFromNode recovers the declaration order of `services:`.
func serviceOrderFromNode(doc *yaml.Node, services map[string]*Service) []string {
	order := make([]string, 0, len(services))
	if doc != nil && doc.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(doc.Content); i += 2 {
			if doc.Content[i].Value != "services" {
				continue
			}
			node := doc.Content[i+1]
			if node.Kind != yaml.MappingNode {
				break
			}
			for j := 0; j+1 < len(node.Content); j += 2 {
				order = append(order, node.Content[j].Value)
			}
			break
		}
	}
	if len(order) == len(services) {
		return order
	}
	// Fall back to a stable alphabetical order.
	order = order[:0]
	for name := range services {
		order = append(order, name)
	}
	sort.Strings(order)
	return order
}

// normalize fills in defaults that the rest of the system relies on.
func (c *Config) normalize() {
	for name, svc := range c.Services {
		if svc == nil {
			svc = &Service{}
			c.Services[name] = svc
		}
		svc.Name = name
		if svc.Runtime == "" {
			svc.Runtime = RuntimeHost
		}
		if svc.CWD == "" {
			svc.CWD = "."
		}
		if svc.Health == nil {
			// Absent health means process health. DevMan never guesses tcp or
			// http from the presence of ports.
			svc.Health = &HealthSpec{Type: HealthProcess}
		} else if svc.Health.Type == "" {
			svc.Health.Type = HealthProcess
		}
		if svc.Restart == nil {
			svc.Restart = &RestartSpec{Policy: RestartNo}
		} else if svc.Restart.Policy == "" {
			svc.Restart.Policy = RestartNo
		}
		for i := range svc.Ports {
			if svc.Ports[i].Range == "" {
				svc.Ports[i].Range = "general"
			}
		}
		for i := range svc.DependsOn {
			if svc.DependsOn[i].Condition == "" {
				svc.DependsOn[i].Condition = ConditionStarted
			}
		}
	}
}

// ServiceNames returns service names in declaration order.
func (c *Config) ServiceNames() []string {
	out := make([]string, 0, len(c.ServiceOrder))
	for _, name := range c.ServiceOrder {
		if _, ok := c.Services[name]; ok {
			out = append(out, name)
		}
	}
	if len(out) == len(c.Services) {
		return out
	}
	out = out[:0]
	for name := range c.Services {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Service looks up a service by name.
func (c *Config) Service(name string) (*Service, error) {
	svc, ok := c.Services[name]
	if !ok {
		return nil, errs.New(errs.CodeServiceNotFound,
			"service %q is not defined in %s", name, filepath.Base(c.ConfigPath))
	}
	return svc, nil
}

// ResolveServiceSet expands a start/stop selector into concrete service names.
//
//	all=true          -> every service
//	profile != ""     -> profiles[profile]
//	explicit non-empty-> the given names, validated
//	otherwise         -> startup.default, or every service when unset
func (c *Config) ResolveServiceSet(explicit []string, profile string, all bool) ([]string, error) {
	switch {
	case len(explicit) > 0:
		for _, name := range explicit {
			if _, err := c.Service(name); err != nil {
				return nil, err
			}
		}
		return explicit, nil
	case all:
		return c.ServiceNames(), nil
	case profile != "":
		names, ok := c.Profiles[profile]
		if !ok {
			return nil, errs.New(errs.CodeConfigInvalid, "profile %q is not defined", profile).
				At("profiles")
		}
		for _, name := range names {
			if _, err := c.Service(name); err != nil {
				return nil, err
			}
		}
		return names, nil
	case c.Startup != nil && len(c.Startup.Default) > 0:
		for _, name := range c.Startup.Default {
			if _, err := c.Service(name); err != nil {
				return nil, err
			}
		}
		return c.Startup.Default, nil
	default:
		return c.ServiceNames(), nil
	}
}

// Execution is the platform-resolved execution shape of a service. Overlays in
// `platform.<os>` are applied on top of the base fields.
type Execution struct {
	Command string
	Args    []string
	CWD     string
	Env     map[string]string
	Shell   ShellSpec
}

// Execution applies the overlay for the given platform key.
func (s *Service) Execution(platform string) Execution {
	exec := Execution{
		Command: s.Command,
		Args:    append([]string(nil), s.Args...),
		CWD:     s.CWD,
		Env:     map[string]string{},
		Shell:   s.Shell,
	}
	for k, v := range s.Env {
		exec.Env[k] = v
	}
	overlay := s.Platform[platform]
	if overlay == nil {
		return exec
	}
	if overlay.Command != "" {
		exec.Command = overlay.Command
	}
	if overlay.Args != nil {
		exec.Args = append([]string(nil), overlay.Args...)
	}
	if overlay.CWD != "" {
		exec.CWD = overlay.CWD
	}
	for k, v := range overlay.Env {
		exec.Env[k] = v
	}
	return exec
}

// AbsCWD resolves a service working directory against the project root.
func (s *Service) AbsCWD(projectRoot, platform string) string {
	cwd := s.Execution(platform).CWD
	if cwd == "" {
		cwd = "."
	}
	if filepath.IsAbs(cwd) {
		return filepath.Clean(cwd)
	}
	return filepath.Clean(filepath.Join(projectRoot, cwd))
}

// PortByName returns the named port spec.
func (s *Service) PortByName(name string) (PortSpec, bool) {
	for _, p := range s.Ports {
		if p.Name == name {
			return p, true
		}
	}
	return PortSpec{}, false
}

// PrimaryPortName returns "http" when declared, otherwise the first port.
func (s *Service) PrimaryPortName() (string, bool) {
	if len(s.Ports) == 0 {
		return "", false
	}
	for _, p := range s.Ports {
		if p.Name == "http" {
			return p.Name, true
		}
	}
	return s.Ports[0].Name, true
}

// Label returns the display name when set, otherwise the service name.
func (s *Service) Label() string {
	if s.DisplayName != "" {
		return s.DisplayName
	}
	return s.Name
}

// Marshal renders the config back to canonical YAML. Used by `devman init`.
func (c *Config) Marshal() ([]byte, error) {
	var sb strings.Builder
	encoder := yaml.NewEncoder(&sb)
	encoder.SetIndent(2)
	if err := encoder.Encode(c); err != nil {
		return nil, fmt.Errorf("cannot encode configuration: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return []byte(sb.String()), nil
}
