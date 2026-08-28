// client.ts is the GUI's single door to the daemon API.
//
// It mirrors internal/client: one place decodes the error envelope, so every page
// can branch on a code (PROJECT_UNTRUSTED, CONFIG_INVALID, PORT_CONFLICT) instead
// of matching on a message.

import type { Endpoint } from "./bridge";
import type {
  ConfigDocument,
  DaemonEvent,
  DaemonStatus,
  LogRecord,
  MachineUsage,
  OperationResult,
  Paths,
  PortAllocation,
  PortUsage,
  Preview,
  Project,
  Selection,
  Service,
  ToolResolution,
  ValidationResult,
} from "./types";

/** ApiError carries the daemon's own code and details. */
export class ApiError extends Error {
  readonly code: string;
  readonly path?: string;
  readonly details?: Record<string, unknown>;

  constructor(code: string, message: string, path?: string, details?: Record<string, unknown>) {
    super(message);
    this.name = "ApiError";
    this.code = code;
    this.path = path;
    this.details = details;
  }

  /** validation extracts the findings the config endpoints attach to a refusal. */
  validation(): ValidationResult | null {
    const found = this.details?.["validation"];
    return found ? (found as ValidationResult) : null;
  }
}

export function errorCode(error: unknown): string {
  return error instanceof ApiError ? error.code : "";
}

export function errorMessage(error: unknown): string {
  if (error instanceof Error) return error.message;
  return String(error);
}

export class Api {
  readonly endpoint: Endpoint;

  constructor(endpoint: Endpoint) {
    this.endpoint = endpoint;
  }

  private async request<T>(method: string, path: string, body?: unknown): Promise<T> {
    const response = await fetch(this.endpoint.base_url + path, {
      method,
      headers: {
        Authorization: `Bearer ${this.endpoint.token}`,
        ...(body === undefined ? {} : { "Content-Type": "application/json" }),
      },
      body: body === undefined ? null : JSON.stringify(body),
    }).catch((cause: unknown) => {
      // Prefixed with the code so the window can say this in the user's language
      // while still showing the browser's own detail.
      throw new ApiError("DAEMON_NOT_RUNNING", `DAEMON_NOT_RUNNING: ${errorMessage(cause)}`);
    });

    if (response.status === 204) return undefined as T;

    const text = await response.text();
    if (!response.ok) {
      throw decodeError(response.status, text);
    }
    if (!text) return undefined as T;
    return JSON.parse(text) as T;
  }

  /** streamUrl builds an SSE URL; EventSource cannot set an Authorization header,
   *  so the two stream endpoints accept the token as a query parameter. */
  streamUrl(path: string, params: Record<string, string | number> = {}): string {
    const url = new URL(this.endpoint.base_url + path);
    url.searchParams.set("token", this.endpoint.token);
    for (const [key, value] of Object.entries(params)) {
      url.searchParams.set(key, String(value));
    }
    return url.toString();
  }

  daemonStatus(): Promise<DaemonStatus> {
    return this.request("GET", "/daemon/status");
  }

  shutdownDaemon(): Promise<OperationResult> {
    return this.request("POST", "/daemon/shutdown");
  }

  paths(): Promise<Paths> {
    return this.request("GET", "/paths");
  }

  settings(): Promise<Record<string, string>> {
    return this.request("GET", "/settings");
  }

  setSetting(key: string, value: string): Promise<Record<string, string>> {
    return this.request("PUT", "/settings", { key, value });
  }

  tools(): Promise<ToolResolution[]> {
    return this.request("GET", "/tools");
  }

  // Host load. Separate from project status because the sidebar shows it on
  // every page, including ones with no project loaded.
  machineUsage(): Promise<MachineUsage> {
    return this.request("GET", "/machine/usage");
  }


  projects(withServices = false): Promise<Project[]> {
    return this.request("GET", withServices ? "/projects?services=true" : "/projects");
  }

  project(id: string): Promise<Project> {
    return this.request("GET", `/projects/${encodeURIComponent(id)}`);
  }

  inspect(path: string): Promise<Preview> {
    return this.request("POST", "/projects/inspect", { path });
  }

  register(path: string, trust: boolean): Promise<Project> {
    return this.request("POST", "/projects", { path, trust });
  }

  unregister(id: string): Promise<void> {
    return this.request("DELETE", `/projects/${encodeURIComponent(id)}`);
  }

  trust(id: string, revoke = false): Promise<Project> {
    return this.request("POST", `/projects/${encodeURIComponent(id)}/trust`, { revoke });
  }

  validate(id: string): Promise<ValidationResult> {
    return this.request("GET", `/projects/${encodeURIComponent(id)}/validate`);
  }

  configFile(id: string): Promise<ConfigDocument> {
    return this.request("GET", `/projects/${encodeURIComponent(id)}/config`);
  }

  saveConfigFile(id: string, content: string): Promise<ConfigDocument> {
    return this.request("PUT", `/projects/${encodeURIComponent(id)}/config`, { content });
  }

  startProject(id: string, selection: Selection = {}): Promise<OperationResult> {
    return this.request("POST", `/projects/${encodeURIComponent(id)}/start`, selection);
  }

  stopProject(id: string, selection: Selection = {}): Promise<OperationResult> {
    return this.request("POST", `/projects/${encodeURIComponent(id)}/stop`, selection);
  }

  restartProject(id: string, selection: Selection = {}): Promise<OperationResult> {
    return this.request("POST", `/projects/${encodeURIComponent(id)}/restart`, selection);
  }

  services(projectID: string): Promise<Service[]> {
    return this.request("GET", `/projects/${encodeURIComponent(projectID)}/services`);
  }

  startService(projectID: string, name: string): Promise<Service> {
    return this.request("POST", `${servicePath(projectID, name)}/start`);
  }

  stopService(projectID: string, name: string): Promise<Service> {
    return this.request("POST", `${servicePath(projectID, name)}/stop`);
  }

  restartService(projectID: string, name: string): Promise<Service> {
    return this.request("POST", `${servicePath(projectID, name)}/restart`);
  }

  logs(projectID: string, name: string, tail = 500): Promise<LogRecord[]> {
    return this.request("GET", `${servicePath(projectID, name)}/logs?tail=${tail}`);
  }

  logStreamUrl(projectID: string, name: string, tail = 200): string {
    return this.streamUrl(`${servicePath(projectID, name)}/logs/stream`, { tail });
  }

  ports(): Promise<PortAllocation[]> {
    return this.request("GET", "/ports");
  }

  portUsage(port: number): Promise<PortUsage> {
    return this.request("GET", `/ports/${port}`);
  }

  events(limit = 100): Promise<DaemonEvent[]> {
    return this.request("GET", `/events?limit=${limit}`);
  }

  eventStreamUrl(replay = 20): string {
    return this.streamUrl("/events/stream", { replay });
  }
}

function servicePath(projectID: string, name: string): string {
  return `/projects/${encodeURIComponent(projectID)}/services/${encodeURIComponent(name)}`;
}

function decodeError(status: number, text: string): ApiError {
  try {
    const parsed = JSON.parse(text) as { error?: { code: string; message: string; path?: string; details?: Record<string, unknown> } };
    if (parsed.error) {
      return new ApiError(parsed.error.code, parsed.error.message, parsed.error.path, parsed.error.details);
    }
  } catch {
    // Falls through to the status-only error below.
  }
  return new ApiError("INTERNAL", `HTTP ${status}`);
}
