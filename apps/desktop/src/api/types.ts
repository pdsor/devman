// The wire contract, mirroring pkg/dto.
//
// These declarations are hand-written rather than generated because they are the
// one place the GUI states what it expects from the daemon; a mismatch should
// show up as a type error here, not as an undefined field in a page.

export type ProcessStatus =
  | "STOPPED"
  | "STARTING"
  | "RUNNING"
  | "STOPPING"
  | "FAILED"
  | "CRASHED"
  | "BLOCKED"
  | "UNKNOWN";

export type DesiredState = "RUNNING" | "STOPPED";

export type HealthStatus = "UNKNOWN" | "CHECKING" | "HEALTHY" | "UNHEALTHY" | "N/A";

export type ProjectStatus =
  | "STOPPED"
  | "STARTING"
  | "HEALTHY"
  | "DEGRADED"
  | "FAILED"
  | "STOPPING";

export type PortStatus = "RESERVED" | "BOUND" | "UNVERIFIED" | "RELEASED" | "CONFLICT";

export type LogCapture = "attached" | "detached" | "none";

export interface ApiErrorBody {
  code: string;
  message: string;
  path?: string;
  details?: Record<string, unknown>;
}

export interface Observability {
  log_capture: LogCapture;
  adopted: boolean;
}

export interface ProjectSummary {
  total: number;
  running: number;
  healthy: number;
  failed: number;
}

// Absent rather than zeroed when DevMan cannot measure something: a compose or
// external service has no host process tree, which is not the same as one that
// is using nothing.
export interface Usage {
  cpu_percent: number;
  memory_bytes: number;
  memory_percent: number;
  procs: number;
  sampled_at: string;
}

export interface MachineUsage {
  cpu_percent: number;
  cores: number;
  memory_used_bytes: number;
  memory_total_bytes: number;
  memory_percent: number;
  sampled_at: string;
}


export interface Project {
  id: string;
  name: string;
  display_name?: string;
  path: string;
  config_path: string;
  favorite: boolean;
  status: ProjectStatus;
  trusted: boolean;
  config_error?: ApiErrorBody;
  services?: Service[];
  summary: ProjectSummary;
  usage?: Usage;

  created_at: string;
  updated_at: string;
  last_started_at?: string;
}

export interface HealthResult {
  status: HealthStatus;
  type: string;
  target?: string;
  checked_at?: string;
  latency_ms?: number;
  consecutive_failures?: number;
  message?: string;
}

export interface PortAllocation {
  port: number;
  name: string;
  project: string;
  service: string;
  env?: string;
  status: PortStatus;
  allocated_at: string;
  released_at?: string;
}

export interface PortOwner {
  pid?: number;
  name?: string;
}

export interface PortUsage {
  port: number;
  occupied: boolean;
  allocation?: PortAllocation;
  owner?: PortOwner;
}

export interface Service {
  project: string;
  name: string;
  display_name?: string;
  runtime: string;
  status: ProcessStatus;
  desired_state: DesiredState;
  health: HealthResult;
  pid?: number;
  started_at?: string;
  uptime_seconds?: number;
  restart_count: number;
  last_exit_code?: number;
  command_line?: string;
  cwd?: string;
  ports?: PortAllocation[];
  url?: string;
  depends_on?: string[];
  observability: Observability;
  usage?: Usage;
  message?: string;
  reason?: ApiErrorBody;
}


export interface OperationResult {
  project: string;
  services: Service[];
  errors?: ApiErrorBody[];
}

export type EventType =
  | "PROJECT_REGISTERED"
  | "PROJECT_UNREGISTERED"
  | "PROJECT_STARTED"
  | "PROJECT_STOPPED"
  | "SERVICE_STARTING"
  | "SERVICE_STARTED"
  | "SERVICE_STOPPING"
  | "SERVICE_STOPPED"
  | "SERVICE_EXITED"
  | "SERVICE_CRASHED"
  | "SERVICE_BLOCKED"
  | "SERVICE_RESTART_SCHEDULED"
  | "SERVICE_ADOPTED"
  | "PORT_RESERVED"
  | "PORT_BOUND"
  | "PORT_RELEASED"
  | "PORT_CONFLICT"
  | "HEALTH_CHANGED"
  | "DAEMON_READY";

export interface DaemonEvent {
  seq: number;
  type: EventType;
  timestamp: string;
  project?: string;
  service?: string;
  message?: string;
  data?: Record<string, unknown>;
}

export interface DaemonInfo {
  pid: number;
  port: number;
  host: string;
  started_at: string;
  api_version: string;
  version?: string;
  graceful_signals: boolean;
}

export interface DaemonStatus {
  info: DaemonInfo;
  uptime_seconds: number;
  projects: number;
  running_services: number;
  data_dir: string;
  logs_dir: string;
  healthy: boolean;
}

export interface Paths {
  home: string;
  settings: string;
  database: string;
  daemon: string;
  auth_token: string;
  logs: string;
}

export interface ToolResolution {
  name: string;
  path?: string;
  found: boolean;
}

export interface LogRecord {
  seq: number;
  timestamp: string;
  project: string;
  service: string;
  stream: string;
  message: string;
}

export interface ValidationResult {
  valid: boolean;
  errors: ApiErrorBody[] | null;
  warnings: ApiErrorBody[] | null;
}

export interface ExecutionSummary {
  service: string;
  runtime: string;
  cwd: string;
  command_line?: string;
  shell?: string;
  env_files?: string[];
  compose?: string;
}

export interface Preview {
  id: string;
  name: string;
  path: string;
  config_path: string;
  execution_fingerprint: string;
  execution: ExecutionSummary[];
  validation: ValidationResult | null;
  already_registered: boolean;
  trust_required: boolean;
}

export interface ConfigDocument {
  path: string;
  content: string;
  validation?: ValidationResult;
  trusted: boolean;
}

export interface Selection {
  services?: string[];
  profile?: string;
  all?: boolean;
}
