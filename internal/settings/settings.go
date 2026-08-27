// Package settings holds the global, machine-local DevMan settings stored in
// <data dir>/config.yaml. Project behaviour lives in devman.yaml; this file is
// only about how this machine runs DevMan.
package settings

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/devman-project/devman/pkg/config"
	"github.com/devman-project/devman/pkg/errs"
	"gopkg.in/yaml.v3"
)

// RangeGeneral is the fallback port range name.
const RangeGeneral = "general"

// Settings is the whole config.yaml document.
type Settings struct {
	Daemon      DaemonSettings        `yaml:"daemon" json:"daemon"`
	PortRanges  map[string]PortRange  `yaml:"port_ranges" json:"port_ranges"`
	Logs        LogSettings           `yaml:"logs" json:"logs"`
	Defaults    DefaultSettings       `yaml:"defaults" json:"defaults"`
	Environment EnvironmentSettings   `yaml:"environment" json:"environment"`
	Startup     StartupSettings       `yaml:"startup" json:"startup"`
}

// DaemonSettings controls the local API listener.
type DaemonSettings struct {
	// Host is always a loopback address. 0.0.0.0 is rejected.
	Host string `yaml:"host" json:"host"`
	// PortStart/PortEnd bound the discovery scan for the API port.
	PortStart int `yaml:"port_start" json:"port_start"`
	PortEnd   int `yaml:"port_end" json:"port_end"`
}

// PortRange is an inclusive allocation window for `value: auto` ports.
type PortRange struct {
	Start int `yaml:"start" json:"start"`
	End   int `yaml:"end" json:"end"`
}

// LogSettings controls on-disk log rotation.
type LogSettings struct {
	MaxSizeMB  int `yaml:"max_size_mb" json:"max_size_mb"`
	MaxBackups int `yaml:"max_backups" json:"max_backups"`
	// RingBuffer is how many structured records are kept in memory per service
	// for instant replay to new log stream subscribers.
	RingBuffer int `yaml:"ring_buffer" json:"ring_buffer"`
}

// DefaultSettings supplies the fallbacks for optional devman.yaml fields.
type DefaultSettings struct {
	GracefulTimeout config.Duration `yaml:"graceful_timeout" json:"graceful_timeout"`
	HealthInterval  config.Duration `yaml:"health_interval" json:"health_interval"`
	HealthTimeout   config.Duration `yaml:"health_timeout" json:"health_timeout"`
	HealthRetries   int             `yaml:"health_retries" json:"health_retries"`
	RestartDelay    config.Duration `yaml:"restart_delay" json:"restart_delay"`
	RestartMaxDelay config.Duration `yaml:"restart_max_delay" json:"restart_max_delay"`
	StartTimeout    config.Duration `yaml:"start_timeout" json:"start_timeout"`
}

// EnvironmentSettings compensates for GUI launches that do not inherit the
// user's interactive shell PATH.
type EnvironmentSettings struct {
	AdditionalPath []string          `yaml:"additional_path" json:"additional_path"`
	Env            map[string]string `yaml:"env" json:"env"`
}

// StartupSettings controls login autostart. Daemon and GUI are separate.
type StartupSettings struct {
	DaemonOnLogin bool `yaml:"daemon_on_login" json:"daemon_on_login"`
	GUIOnLogin    bool `yaml:"gui_on_login" json:"gui_on_login"`
}

// Default returns the built-in settings.
func Default() *Settings {
	return &Settings{
		Daemon: DaemonSettings{Host: "127.0.0.1", PortStart: 39100, PortEnd: 39149},
		PortRanges: map[string]PortRange{
			"frontend":   {Start: 3000, End: 3999},
			"backend":    {Start: 8000, End: 8999},
			RangeGeneral: {Start: 10000, End: 19999},
		},
		Logs: LogSettings{MaxSizeMB: 10, MaxBackups: 5, RingBuffer: 2000},
		Defaults: DefaultSettings{
			GracefulTimeout: config.Duration{Duration: 10 * time.Second},
			HealthInterval:  config.Duration{Duration: 5 * time.Second},
			HealthTimeout:   config.Duration{Duration: 3 * time.Second},
			HealthRetries:   10,
			RestartDelay:    config.Duration{Duration: time.Second},
			RestartMaxDelay: config.Duration{Duration: 30 * time.Second},
			StartTimeout:    config.Duration{Duration: 60 * time.Second},
		},
		Environment: EnvironmentSettings{Env: map[string]string{}},
		Startup:     StartupSettings{},
	}
}

// Load reads config.yaml, filling in defaults for anything absent. A missing
// file is not an error: defaults are returned.
func Load(path string) (*Settings, error) {
	s := Default()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, errs.Wrap(errs.CodeInternal, err, "cannot read %s", path)
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)
	if err := decoder.Decode(s); err != nil {
		return nil, errs.Wrap(errs.CodeConfigInvalid, err, "invalid settings in %s", path)
	}
	s.applyDefaults()
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return s, nil
}

// Save writes config.yaml atomically.
func (s *Settings) Save(path string) error {
	if err := s.Validate(); err != nil {
		return err
	}
	var sb strings.Builder
	encoder := yaml.NewEncoder(&sb)
	encoder.SetIndent(2)
	if err := encoder.Encode(s); err != nil {
		return errs.Wrap(errs.CodeInternal, err, "cannot encode settings")
	}
	if err := encoder.Close(); err != nil {
		return errs.Wrap(errs.CodeInternal, err, "cannot encode settings")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return errs.Wrap(errs.CodeInternal, err, "cannot create settings directory")
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(sb.String()), 0o600); err != nil {
		return errs.Wrap(errs.CodeInternal, err, "cannot write settings")
	}
	if err := os.Rename(tmp, path); err != nil {
		return errs.Wrap(errs.CodeInternal, err, "cannot replace settings")
	}
	return nil
}

func (s *Settings) applyDefaults() {
	d := Default()
	if s.Daemon.Host == "" {
		s.Daemon.Host = d.Daemon.Host
	}
	if s.Daemon.PortStart == 0 {
		s.Daemon.PortStart = d.Daemon.PortStart
	}
	if s.Daemon.PortEnd == 0 {
		s.Daemon.PortEnd = d.Daemon.PortEnd
	}
	if s.PortRanges == nil {
		s.PortRanges = d.PortRanges
	}
	if _, ok := s.PortRanges[RangeGeneral]; !ok {
		s.PortRanges[RangeGeneral] = d.PortRanges[RangeGeneral]
	}
	if s.Logs.MaxSizeMB == 0 {
		s.Logs.MaxSizeMB = d.Logs.MaxSizeMB
	}
	if s.Logs.MaxBackups == 0 {
		s.Logs.MaxBackups = d.Logs.MaxBackups
	}
	if s.Logs.RingBuffer == 0 {
		s.Logs.RingBuffer = d.Logs.RingBuffer
	}
	if s.Defaults.GracefulTimeout.Duration == 0 {
		s.Defaults.GracefulTimeout = d.Defaults.GracefulTimeout
	}
	if s.Defaults.HealthInterval.Duration == 0 {
		s.Defaults.HealthInterval = d.Defaults.HealthInterval
	}
	if s.Defaults.HealthTimeout.Duration == 0 {
		s.Defaults.HealthTimeout = d.Defaults.HealthTimeout
	}
	if s.Defaults.HealthRetries == 0 {
		s.Defaults.HealthRetries = d.Defaults.HealthRetries
	}
	if s.Defaults.RestartDelay.Duration == 0 {
		s.Defaults.RestartDelay = d.Defaults.RestartDelay
	}
	if s.Defaults.RestartMaxDelay.Duration == 0 {
		s.Defaults.RestartMaxDelay = d.Defaults.RestartMaxDelay
	}
	if s.Defaults.StartTimeout.Duration == 0 {
		s.Defaults.StartTimeout = d.Defaults.StartTimeout
	}
	if s.Environment.Env == nil {
		s.Environment.Env = map[string]string{}
	}
}

// Validate rejects settings that would make the daemon unsafe or unusable.
func (s *Settings) Validate() error {
	if !isLoopback(s.Daemon.Host) {
		return errs.New(errs.CodeConfigInvalid,
			"daemon.host must be a loopback address, got %q", s.Daemon.Host)
	}
	if s.Daemon.PortStart < 1 || s.Daemon.PortEnd > 65535 || s.Daemon.PortStart > s.Daemon.PortEnd {
		return errs.New(errs.CodeConfigInvalid, "daemon port range %d-%d is invalid",
			s.Daemon.PortStart, s.Daemon.PortEnd)
	}
	for name, r := range s.PortRanges {
		if r.Start < 1 || r.End > 65535 || r.Start > r.End {
			return errs.New(errs.CodeConfigInvalid,
				"port_ranges.%s (%d-%d) is invalid", name, r.Start, r.End)
		}
	}
	if s.Logs.MaxSizeMB < 1 || s.Logs.MaxBackups < 0 || s.Logs.RingBuffer < 1 {
		return errs.New(errs.CodeConfigInvalid, "logs settings are invalid")
	}
	return nil
}

func isLoopback(host string) bool {
	switch host {
	case "127.0.0.1", "::1", "localhost":
		return true
	default:
		return false
	}
}

// Range returns the named range, falling back to general.
func (s *Settings) Range(name string) PortRange {
	if name == "" {
		name = RangeGeneral
	}
	if r, ok := s.PortRanges[name]; ok {
		return r
	}
	return s.PortRanges[RangeGeneral]
}

// HasRange reports whether a range name is defined.
func (s *Settings) HasRange(name string) bool {
	_, ok := s.PortRanges[name]
	return ok
}

// Flatten renders the settings as sorted dotted key/value pairs for
// `devman config list`.
func (s *Settings) Flatten() map[string]string {
	var raw map[string]any
	data, err := yaml.Marshal(s)
	if err != nil {
		return nil
	}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil
	}
	out := map[string]string{}
	flatten("", raw, out)
	return out
}

func flatten(prefix string, value any, out map[string]string) {
	switch typed := value.(type) {
	case map[string]any:
		for _, key := range sortedKeys(typed) {
			flatten(join(prefix, key), typed[key], out)
		}
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			parts = append(parts, fmt.Sprint(item))
		}
		out[prefix] = strings.Join(parts, ",")
	case nil:
		out[prefix] = ""
	default:
		out[prefix] = fmt.Sprint(typed)
	}
}

func join(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Get reads a dotted key such as "port_ranges.frontend.start".
func (s *Settings) Get(key string) (string, error) {
	flat := s.Flatten()
	if value, ok := flat[key]; ok {
		return value, nil
	}
	return "", errs.New(errs.CodeInvalidRequest, "unknown setting %q", key)
}

// Set writes a dotted key. The value is parsed as YAML so booleans, numbers
// and comma-free strings all work; the result is re-validated before it is
// returned so an invalid edit can never be persisted.
func (s *Settings) Set(key, value string) error {
	if key == "" {
		return errs.New(errs.CodeInvalidRequest, "setting key is required")
	}
	data, err := yaml.Marshal(s)
	if err != nil {
		return errs.Wrap(errs.CodeInternal, err, "cannot encode settings")
	}
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return errs.Wrap(errs.CodeInternal, err, "cannot decode settings")
	}

	parts := strings.Split(key, ".")
	cursor := raw
	for i, part := range parts[:len(parts)-1] {
		next, ok := cursor[part]
		if !ok {
			return errs.New(errs.CodeInvalidRequest, "unknown setting %q", strings.Join(parts[:i+1], "."))
		}
		nested, ok := next.(map[string]any)
		if !ok {
			return errs.New(errs.CodeInvalidRequest, "%q is not a settings group", strings.Join(parts[:i+1], "."))
		}
		cursor = nested
	}
	leaf := parts[len(parts)-1]
	if _, ok := cursor[leaf]; !ok {
		// Allow creating new port ranges, but nothing else.
		if len(parts) < 2 || parts[0] != "port_ranges" {
			return errs.New(errs.CodeInvalidRequest, "unknown setting %q", key)
		}
	}
	cursor[leaf] = parseScalar(value)

	updated, err := yaml.Marshal(raw)
	if err != nil {
		return errs.Wrap(errs.CodeInternal, err, "cannot encode settings")
	}
	candidate := Default()
	decoder := yaml.NewDecoder(strings.NewReader(string(updated)))
	decoder.KnownFields(true)
	if err := decoder.Decode(candidate); err != nil {
		return errs.Wrap(errs.CodeInvalidRequest, err, "cannot apply %s=%s", key, value)
	}
	candidate.applyDefaults()
	if err := candidate.Validate(); err != nil {
		return err
	}
	*s = *candidate
	return nil
}

func parseScalar(value string) any {
	if b, err := strconv.ParseBool(value); err == nil {
		return b
	}
	if n, err := strconv.Atoi(value); err == nil {
		return n
	}
	if strings.Contains(value, ",") {
		parts := strings.Split(value, ",")
		out := make([]any, 0, len(parts))
		for _, part := range parts {
			trimmed := strings.TrimSpace(part)
			if trimmed != "" {
				out = append(out, trimmed)
			}
		}
		return out
	}
	return value
}
