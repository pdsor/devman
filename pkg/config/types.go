// Package config implements the canonical devman.yaml schema: parsing,
// normalisation, validation and the execution fingerprint used for trust.
//
// The V0.1 schema deliberately has exactly one canonical spelling for every
// concept. Sugar forms are not accepted so that the parser, the JSON schema,
// the docs and the Codex skill can never drift apart.
package config

import (
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// SchemaVersion is the only supported devman.yaml version in V0.1.
const SchemaVersion = 1

// RuntimeKind selects which Runtime implementation owns a service.
type RuntimeKind string

const (
	RuntimeHost           RuntimeKind = "host"
	RuntimeDockerCompose  RuntimeKind = "docker-compose"
	RuntimeExternal       RuntimeKind = "external"
)

// HealthKind selects the health probe implementation.
type HealthKind string

const (
	HealthProcess HealthKind = "process"
	HealthTCP     HealthKind = "tcp"
	HealthHTTP    HealthKind = "http"
)

// RestartPolicy controls automatic restarts after an unexpected exit.
type RestartPolicy string

const (
	RestartNo        RestartPolicy = "no"
	RestartOnFailure RestartPolicy = "on-failure"
	RestartAlways    RestartPolicy = "always"
)

// DependencyCondition is the gate a dependency must satisfy before a
// dependent service is started.
type DependencyCondition string

const (
	ConditionStarted DependencyCondition = "started"
	ConditionHealthy DependencyCondition = "healthy"
)

// ShellType selects the interpreter used when Shell.Enabled is true.
type ShellType string

const (
	ShellDefault    ShellType = ""
	ShellPowerShell ShellType = "powershell"
)

// Platform overlay keys.
const (
	PlatformWindows = "windows"
	PlatformMacOS   = "macos"
	PlatformLinux   = "linux"
)

// CurrentPlatform maps runtime.GOOS onto a devman.yaml platform overlay key.
func CurrentPlatform() string {
	switch runtime.GOOS {
	case "windows":
		return PlatformWindows
	case "darwin":
		return PlatformMacOS
	default:
		return PlatformLinux
	}
}

// Config is a parsed devman.yaml.
type Config struct {
	Version  int                 `yaml:"version" json:"version"`
	Project  ProjectSpec         `yaml:"project" json:"project"`
	Services map[string]*Service `yaml:"services" json:"services"`
	Startup  *StartupSpec        `yaml:"startup,omitempty" json:"startup,omitempty"`
	Profiles map[string][]string `yaml:"profiles,omitempty" json:"profiles,omitempty"`

	// Source metadata, populated by Load. Never present in YAML.
	ConfigPath  string `yaml:"-" json:"config_path,omitempty"`
	ProjectRoot string `yaml:"-" json:"project_root,omitempty"`

	// ServiceOrder preserves the declaration order of Services, which a Go
	// map cannot. Used only for stable presentation.
	ServiceOrder []string `yaml:"-" json:"service_order,omitempty"`
}

// ProjectSpec is the `project:` block.
type ProjectSpec struct {
	Name        string `yaml:"name" json:"name"`
	DisplayName string `yaml:"display_name,omitempty" json:"display_name,omitempty"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
}

// StartupSpec is the `startup:` block. `default` is the service set used by
// a bare `devman start`.
type StartupSpec struct {
	Default []string `yaml:"default,omitempty" json:"default,omitempty"`
}

// Service is one managed unit of a project.
type Service struct {
	// Name is the map key in `services:`, injected by the parser.
	Name string `yaml:"-" json:"name"`

	DisplayName string      `yaml:"display_name,omitempty" json:"display_name,omitempty"`
	Runtime     RuntimeKind `yaml:"runtime,omitempty" json:"runtime"`

	CWD     string    `yaml:"cwd,omitempty" json:"cwd,omitempty"`
	Command string    `yaml:"command,omitempty" json:"command,omitempty"`
	Args    []string  `yaml:"args,omitempty" json:"args,omitempty"`
	Shell   ShellSpec `yaml:"shell,omitempty" json:"shell"`

	EnvFile     []string          `yaml:"env_file,omitempty" json:"env_file,omitempty"`
	Env         map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
	RequiredEnv []string          `yaml:"required_env,omitempty" json:"required_env,omitempty"`

	Ports     []PortSpec   `yaml:"ports,omitempty" json:"ports,omitempty"`
	DependsOn DependsOn    `yaml:"depends_on,omitempty" json:"depends_on,omitempty"`
	Health    *HealthSpec  `yaml:"health,omitempty" json:"health,omitempty"`
	Restart   *RestartSpec `yaml:"restart,omitempty" json:"restart,omitempty"`

	Autostart       bool      `yaml:"autostart,omitempty" json:"autostart"`
	GracefulTimeout *Duration `yaml:"graceful_timeout,omitempty" json:"graceful_timeout,omitempty"`

	Compose  *ComposeSpec                `yaml:"compose,omitempty" json:"compose,omitempty"`
	Platform map[string]*PlatformOverlay `yaml:"platform,omitempty" json:"platform,omitempty"`
}

// PlatformOverlay narrows execution details for one OS. It is the only way to
// express platform differences; `command` itself is never a map.
type PlatformOverlay struct {
	Command string            `yaml:"command,omitempty" json:"command,omitempty"`
	Args    []string          `yaml:"args,omitempty" json:"args,omitempty"`
	CWD     string            `yaml:"cwd,omitempty" json:"cwd,omitempty"`
	Env     map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
}

// ComposeSpec configures the docker-compose runtime.
type ComposeSpec struct {
	File    string `yaml:"file,omitempty" json:"file,omitempty"`
	Service string `yaml:"service,omitempty" json:"service,omitempty"`
	Project string `yaml:"project,omitempty" json:"project,omitempty"`
}

// PortSpec declares one port owned by a service.
type PortSpec struct {
	Name      string    `yaml:"name" json:"name"`
	Value     PortValue `yaml:"value" json:"value"`
	Preferred int       `yaml:"preferred,omitempty" json:"preferred,omitempty"`
	Env       string    `yaml:"env,omitempty" json:"env,omitempty"`
	Range     string    `yaml:"range,omitempty" json:"range,omitempty"`
}

// HealthSpec declares a health probe. Absent health means HealthProcess;
// DevMan never infers tcp/http from the presence of ports.
type HealthSpec struct {
	Type           HealthKind `yaml:"type" json:"type"`
	URL            string     `yaml:"url,omitempty" json:"url,omitempty"`
	Host           string     `yaml:"host,omitempty" json:"host,omitempty"`
	Port           string     `yaml:"port,omitempty" json:"port,omitempty"`
	ExpectedStatus []int      `yaml:"expected_status,omitempty" json:"expected_status,omitempty"`
	Interval       *Duration  `yaml:"interval,omitempty" json:"interval,omitempty"`
	Timeout        *Duration  `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	Retries        int        `yaml:"retries,omitempty" json:"retries,omitempty"`
}

// RestartSpec configures the automatic restart behaviour on unexpected exit.
type RestartSpec struct {
	Policy      RestartPolicy `yaml:"policy" json:"policy"`
	MaxAttempts int           `yaml:"max_attempts,omitempty" json:"max_attempts,omitempty"`
	Delay       *Duration     `yaml:"delay,omitempty" json:"delay,omitempty"`
	MaxDelay    *Duration     `yaml:"max_delay,omitempty" json:"max_delay,omitempty"`
}

// Dependency is one normalised depends_on entry.
type Dependency struct {
	Name      string              `yaml:"-" json:"name"`
	Condition DependencyCondition `yaml:"condition,omitempty" json:"condition"`
}

// DependsOn is an ordered, normalised dependency list. Both the sequence form
//
//	depends_on: [redis]
//
// and the mapping form
//
//	depends_on:
//	  redis:
//	    condition: started
//
// decode into this type.
type DependsOn []Dependency

// UnmarshalYAML accepts both the sequence and the mapping form.
func (d *DependsOn) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.SequenceNode:
		out := make(DependsOn, 0, len(node.Content))
		for _, item := range node.Content {
			var name string
			if err := item.Decode(&name); err != nil {
				return fmt.Errorf("depends_on entries must be service names: %w", err)
			}
			out = append(out, Dependency{Name: name, Condition: ConditionStarted})
		}
		*d = out
		return nil
	case yaml.MappingNode:
		out := make(DependsOn, 0, len(node.Content)/2)
		for i := 0; i+1 < len(node.Content); i += 2 {
			name := node.Content[i].Value
			dep := Dependency{Name: name, Condition: ConditionStarted}
			value := node.Content[i+1]
			if value.Kind == yaml.MappingNode {
				var body struct {
					Condition string `yaml:"condition"`
				}
				if err := value.Decode(&body); err != nil {
					return err
				}
				if body.Condition != "" {
					dep.Condition = DependencyCondition(body.Condition)
				}
			} else if value.Tag != "!!null" {
				return fmt.Errorf("depends_on.%s must be a mapping with a condition", name)
			}
			out = append(out, dep)
		}
		*d = out
		return nil
	case yaml.AliasNode:
		return d.UnmarshalYAML(node.Alias)
	default:
		return fmt.Errorf("depends_on must be a list or a mapping")
	}
}

// MarshalYAML always emits the canonical mapping form.
func (d DependsOn) MarshalYAML() (any, error) {
	if len(d) == 0 {
		return nil, nil
	}
	node := &yaml.Node{Kind: yaml.MappingNode}
	for _, dep := range d {
		node.Content = append(node.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: dep.Name},
			&yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
				{Kind: yaml.ScalarNode, Value: "condition"},
				{Kind: yaml.ScalarNode, Value: string(dep.Condition)},
			}},
		)
	}
	return node, nil
}

// ShellSpec is `shell: false`, `shell: true` or `shell: {type: powershell}`.
type ShellSpec struct {
	Enabled bool      `json:"enabled"`
	Type    ShellType `json:"type,omitempty"`
}

// UnmarshalYAML accepts a bool or a mapping.
func (s *ShellSpec) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		if node.Tag == "!!null" {
			*s = ShellSpec{}
			return nil
		}
		var enabled bool
		if err := node.Decode(&enabled); err != nil {
			return fmt.Errorf("shell must be a boolean or a mapping with a type")
		}
		*s = ShellSpec{Enabled: enabled}
		return nil
	case yaml.MappingNode:
		var body struct {
			Type string `yaml:"type"`
		}
		if err := node.Decode(&body); err != nil {
			return err
		}
		*s = ShellSpec{Enabled: true, Type: ShellType(body.Type)}
		return nil
	case yaml.AliasNode:
		return s.UnmarshalYAML(node.Alias)
	default:
		return fmt.Errorf("shell must be a boolean or a mapping with a type")
	}
}

// MarshalYAML emits `false`, `true` or `{type: ...}`.
func (s ShellSpec) MarshalYAML() (any, error) {
	if s.Type != ShellDefault {
		return map[string]string{"type": string(s.Type)}, nil
	}
	return s.Enabled, nil
}

// PortValue is either `auto` or a fixed port number.
type PortValue struct {
	Auto   bool `json:"auto"`
	Number int  `json:"number,omitempty"`
}

// UnmarshalYAML accepts `auto` or an integer.
func (p *PortValue) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.AliasNode {
		return p.UnmarshalYAML(node.Alias)
	}
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("port value must be `auto` or a port number")
	}
	raw := strings.TrimSpace(node.Value)
	if strings.EqualFold(raw, "auto") {
		*p = PortValue{Auto: true}
		return nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return fmt.Errorf("port value must be `auto` or a port number, got %q", raw)
	}
	*p = PortValue{Number: n}
	return nil
}

// MarshalYAML emits `auto` or the number.
func (p PortValue) MarshalYAML() (any, error) {
	if p.Auto {
		return "auto", nil
	}
	return p.Number, nil
}

func (p PortValue) String() string {
	if p.Auto {
		return "auto"
	}
	return strconv.Itoa(p.Number)
}

// Duration is a YAML/JSON friendly time.Duration accepting "10s" or a plain
// number of seconds.
type Duration struct {
	time.Duration
}

// NewDuration is a helper for defaults and tests.
func NewDuration(d time.Duration) *Duration { return &Duration{Duration: d} }

// UnmarshalYAML accepts "1500ms", "10s", "2m" or 10.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.AliasNode {
		return d.UnmarshalYAML(node.Alias)
	}
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("duration must be a string like \"10s\" or a number of seconds")
	}
	raw := strings.TrimSpace(node.Value)
	if raw == "" {
		return fmt.Errorf("duration must not be empty")
	}
	if n, err := strconv.Atoi(raw); err == nil {
		d.Duration = time.Duration(n) * time.Second
		return nil
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("invalid duration %q: expected a form like \"10s\"", raw)
	}
	d.Duration = parsed
	return nil
}

// MarshalYAML emits the Go duration string.
func (d Duration) MarshalYAML() (any, error) { return d.Duration.String(), nil }

// MarshalJSON emits the Go duration string so DTOs stay human readable.
func (d Duration) MarshalJSON() ([]byte, error) {
	return []byte(strconv.Quote(d.Duration.String())), nil
}

// UnmarshalJSON accepts the string form emitted by MarshalJSON.
func (d *Duration) UnmarshalJSON(data []byte) error {
	raw, err := strconv.Unquote(string(data))
	if err != nil {
		// Tolerate a bare number of nanoseconds.
		n, convErr := strconv.ParseInt(string(data), 10, 64)
		if convErr != nil {
			return err
		}
		d.Duration = time.Duration(n)
		return nil
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return err
	}
	d.Duration = parsed
	return nil
}

// Or returns the duration, or fallback when unset.
func (d *Duration) Or(fallback time.Duration) time.Duration {
	if d == nil || d.Duration <= 0 {
		return fallback
	}
	return d.Duration
}
