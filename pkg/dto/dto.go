// Package dto defines the stable objects exchanged by the daemon API, the CLI
// and the Codex skill.
//
// These types are the contract. CLI table output is only a rendering of them;
// nothing in DevMan ever parses terminal text to discover state.
package dto

import "time"

// ProcessStatus is the observed state of a service's process.
type ProcessStatus string

const (
	StatusStopped  ProcessStatus = "STOPPED"
	StatusStarting ProcessStatus = "STARTING"
	StatusRunning  ProcessStatus = "RUNNING"
	StatusStopping ProcessStatus = "STOPPING"
	// StatusFailed means the service could not be started.
	StatusFailed ProcessStatus = "FAILED"
	// StatusCrashed means it was running and then exited unexpectedly.
	StatusCrashed ProcessStatus = "CRASHED"
	// StatusBlocked means a precondition is unmet (missing Docker, missing
	// required env, a port conflict). It is deliberately distinct from FAILED:
	// nothing was attempted and nothing is broken.
	StatusBlocked ProcessStatus = "BLOCKED"
	StatusUnknown ProcessStatus = "UNKNOWN"
)

// DesiredState is what the user (or an agent) asked for. Restart policies only
// apply while the desired state is RUNNING, which is what keeps a manual
// `devman stop` from racing against an automatic restart.
type DesiredState string

const (
	DesiredRunning DesiredState = "RUNNING"
	DesiredStopped DesiredState = "STOPPED"
)

// HealthStatus is reported separately from ProcessStatus: a process can be
// RUNNING and UNHEALTHY at the same time, and that distinction is the whole
// point of the health system.
type HealthStatus string

const (
	HealthUnknown   HealthStatus = "UNKNOWN"
	HealthChecking  HealthStatus = "CHECKING"
	HealthHealthy   HealthStatus = "HEALTHY"
	HealthUnhealthy HealthStatus = "UNHEALTHY"
	// HealthNotApplicable is used for services with process-only health.
	HealthNotApplicable HealthStatus = "N/A"
)

// ProjectStatus aggregates the services of a project.
type ProjectStatus string

const (
	ProjectStopped  ProjectStatus = "STOPPED"
	ProjectStarting ProjectStatus = "STARTING"
	ProjectHealthy  ProjectStatus = "HEALTHY"
	ProjectDegraded ProjectStatus = "DEGRADED"
	ProjectFailed   ProjectStatus = "FAILED"
	ProjectStopping ProjectStatus = "STOPPING"
)

// PortStatus is the lifecycle of a port allocation.
type PortStatus string

const (
	// PortReserved means DevMan has claimed the port but has not yet seen the
	// service listen on it.
	PortReserved PortStatus = "RESERVED"
	// PortBound means a listener was observed.
	PortBound PortStatus = "BOUND"
	// PortUnverified means the service is running but never bound the port it
	// was given. DevMan warns rather than killing the process.
	PortUnverified PortStatus = "UNVERIFIED"
	PortReleased   PortStatus = "RELEASED"
	PortConflict   PortStatus = "CONFLICT"
)

// LogCapture reports whether the daemon still owns a service's output pipes.
// After a daemon restart, adopted processes keep running but their stdout is
// gone until the service is restarted.
type LogCapture string

const (
	LogCaptureAttached LogCapture = "attached"
	LogCaptureDetached LogCapture = "detached"
	LogCaptureNone     LogCapture = "none"
)

// Observability groups the "how well can DevMan see this" flags, keeping them
// out of the main status enum.
type Observability struct {
	LogCapture LogCapture `json:"log_capture"`
	// Adopted is true when the process survived a daemon restart and was
	// reattached by reconciliation rather than started by this daemon.
	Adopted bool `json:"adopted"`
}

// Project is a registered project.
type Project struct {
	ID          string        `json:"id"`
	Name        string        `json:"name"`
	DisplayName string        `json:"display_name,omitempty"`
	Path        string        `json:"path"`
	ConfigPath  string        `json:"config_path"`
	Favorite    bool          `json:"favorite"`
	Status      ProjectStatus `json:"status"`
	// Trusted reports whether the current execution fingerprint has been
	// approved. An untrusted project cannot be started.
	Trusted bool `json:"trusted"`
	// ConfigError is set when devman.yaml is currently invalid or unreadable.
	ConfigError *Error `json:"config_error,omitempty"`

	Services []Service      `json:"services,omitempty"`
	Summary  ProjectSummary `json:"summary"`

	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	LastStartedAt *time.Time `json:"last_started_at,omitempty"`
}

// ProjectSummary is the headline the GUI shows on a project card.
type ProjectSummary struct {
	Total   int `json:"total"`
	Running int `json:"running"`
	Healthy int `json:"healthy"`
	Failed  int `json:"failed"`
}

// Service is one service with both its declaration and its runtime state.
type Service struct {
	Project     string `json:"project"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name,omitempty"`
	Runtime     string `json:"runtime"`

	Status       ProcessStatus `json:"status"`
	DesiredState DesiredState  `json:"desired_state"`
	Health       HealthResult  `json:"health"`

	PID           int        `json:"pid,omitempty"`
	StartedAt     *time.Time `json:"started_at,omitempty"`
	UptimeSeconds int64      `json:"uptime_seconds,omitempty"`
	RestartCount  int        `json:"restart_count"`
	LastExitCode  *int       `json:"last_exit_code,omitempty"`

	CommandLine   string           `json:"command_line,omitempty"`
	CWD           string           `json:"cwd,omitempty"`
	Ports         []PortAllocation `json:"ports,omitempty"`
	URL           string           `json:"url,omitempty"`
	DependsOn     []string         `json:"depends_on,omitempty"`
	Observability Observability    `json:"observability"`

	// Message explains a BLOCKED or FAILED status in one line.
	Message string `json:"message,omitempty"`
	// Reason carries the machine readable cause for BLOCKED or FAILED.
	Reason *Error `json:"reason,omitempty"`
}

// PortAllocation is one port owned by a service.
type PortAllocation struct {
	Port        int        `json:"port"`
	Name        string     `json:"name"`
	Project     string     `json:"project"`
	Service     string     `json:"service"`
	EnvVar      string     `json:"env,omitempty"`
	Status      PortStatus `json:"status"`
	AllocatedAt time.Time  `json:"allocated_at"`
	ReleasedAt  *time.Time `json:"released_at,omitempty"`
}

// PortUsage describes who holds a port, including processes DevMan does not
// manage. Owner is best effort: on some platforms the owning PID cannot be
// resolved, and that must never block a start.
type PortUsage struct {
	Port       int             `json:"port"`
	Occupied   bool            `json:"occupied"`
	Allocation *PortAllocation `json:"allocation,omitempty"`
	Owner      *PortOwner      `json:"owner,omitempty"`
}

// PortOwner is an external process holding a port.
type PortOwner struct {
	PID  int    `json:"pid,omitempty"`
	Name string `json:"name,omitempty"`
}

// HealthResult is the outcome of the most recent health probe.
type HealthResult struct {
	Status HealthStatus `json:"status"`
	Type   string       `json:"type"`
	// Target is the probed endpoint, e.g. "http://127.0.0.1:8012/health".
	Target    string     `json:"target,omitempty"`
	CheckedAt *time.Time `json:"checked_at,omitempty"`
	LatencyMS int64      `json:"latency_ms,omitempty"`
	Failures  int        `json:"consecutive_failures,omitempty"`
	Message   string     `json:"message,omitempty"`
}

// ProcessInstance is one historical run of a service.
type ProcessInstance struct {
	ID           string        `json:"id"`
	Project      string        `json:"project"`
	Service      string        `json:"service"`
	PID          int           `json:"pid"`
	Status       ProcessStatus `json:"status"`
	Runtime      string        `json:"runtime"`
	CommandLine  string        `json:"command_line,omitempty"`
	CWD          string        `json:"cwd,omitempty"`
	StartedAt    time.Time     `json:"started_at"`
	StoppedAt    *time.Time    `json:"stopped_at,omitempty"`
	ExitCode     *int          `json:"exit_code,omitempty"`
	RestartCount int           `json:"restart_count"`
}

// Error is the wire form of pkg/errs.Error. It is duplicated here so DTO
// consumers need only one package.
type Error struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Path    string         `json:"path,omitempty"`
	Details map[string]any `json:"details,omitempty"`
}

// EventType enumerates the daemon's structured events.
type EventType string

const (
	EventProjectRegistered   EventType = "PROJECT_REGISTERED"
	EventProjectUnregistered EventType = "PROJECT_UNREGISTERED"
	EventProjectStarted      EventType = "PROJECT_STARTED"
	EventProjectStopped      EventType = "PROJECT_STOPPED"

	EventServiceStarting         EventType = "SERVICE_STARTING"
	EventServiceStarted          EventType = "SERVICE_STARTED"
	EventServiceStopping         EventType = "SERVICE_STOPPING"
	EventServiceStopped          EventType = "SERVICE_STOPPED"
	EventServiceExited           EventType = "SERVICE_EXITED"
	EventServiceCrashed          EventType = "SERVICE_CRASHED"
	EventServiceBlocked          EventType = "SERVICE_BLOCKED"
	EventServiceRestartScheduled EventType = "SERVICE_RESTART_SCHEDULED"
	EventServiceAdopted          EventType = "SERVICE_ADOPTED"

	EventPortReserved EventType = "PORT_RESERVED"
	EventPortBound    EventType = "PORT_BOUND"
	EventPortReleased EventType = "PORT_RELEASED"
	EventPortConflict EventType = "PORT_CONFLICT"

	EventHealthChanged EventType = "HEALTH_CHANGED"

	EventDaemonReady EventType = "DAEMON_READY"
)

// Event is one state change published on the daemon event bus and streamed to
// subscribers over SSE.
type Event struct {
	Seq       uint64         `json:"seq"`
	Type      EventType      `json:"type"`
	Timestamp time.Time      `json:"timestamp"`
	Project   string         `json:"project,omitempty"`
	Service   string         `json:"service,omitempty"`
	Message   string         `json:"message,omitempty"`
	Data      map[string]any `json:"data,omitempty"`
}

// DaemonInfo is written to daemon.json for discovery. The auth token is
// deliberately kept in a separate file with tighter permissions.
type DaemonInfo struct {
	PID        int       `json:"pid"`
	Port       int       `json:"port"`
	Host       string    `json:"host"`
	StartedAt  time.Time `json:"started_at"`
	APIVersion string    `json:"api_version"`
	Version    string    `json:"version,omitempty"`
	// GracefulSignals reports whether graceful shutdown is available. On
	// Windows without a console it is false and stops become force kills.
	GracefulSignals bool `json:"graceful_signals"`
}

// DaemonStatus is the response of the daemon status endpoint.
type DaemonStatus struct {
	Info     DaemonInfo `json:"info"`
	Uptime   int64      `json:"uptime_seconds"`
	Projects int        `json:"projects"`
	Running  int        `json:"running_services"`
	DataDir  string     `json:"data_dir"`
	LogsDir  string     `json:"logs_dir"`
	Healthy  bool       `json:"healthy"`
}

// Paths is the response of `devman paths`.
type Paths struct {
	Home      string `json:"home"`
	Settings  string `json:"settings"`
	Database  string `json:"database"`
	Daemon    string `json:"daemon"`
	AuthToken string `json:"auth_token"`
	Logs      string `json:"logs"`
}

// ToolResolution reports where a development tool was found, for the
// Environment page and for diagnosing GUI launches with a reduced PATH.
type ToolResolution struct {
	Name  string `json:"name"`
	Path  string `json:"path,omitempty"`
	Found bool   `json:"found"`
}

// EnvVarStatus reports whether an environment variable is configured, never its
// value. Secrets must not leak into the GUI or into logs.
type EnvVarStatus struct {
	Name       string `json:"name"`
	Configured bool   `json:"configured"`
	Source     string `json:"source,omitempty"`
}

// OperationResult is returned by start/stop/restart calls.
type OperationResult struct {
	Project  string    `json:"project"`
	Services []Service `json:"services"`
	Errors   []Error   `json:"errors,omitempty"`
}
