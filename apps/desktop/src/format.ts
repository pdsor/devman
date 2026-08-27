// Formatting helpers. Every function here renders something the daemon reported,
// so the output stays literal: no rounding that hides a value, no "a while ago"
// where a timestamp is what a developer needs.

import type {
  HealthStatus,
  ProcessStatus,
  ProjectStatus,
  Service,
  PortStatus,
} from "./api/types";
import type { Translate } from "./i18n";
import type { MessageKey } from "./i18n/messages";

export type Tone = "ok" | "warn" | "bad" | "blocked" | "idle" | "info";

export function toneClass(tone: Tone): string {
  return `t-${tone}`;
}

export function processTone(status: ProcessStatus): Tone {
  switch (status) {
    case "RUNNING":
      return "ok";
    case "STARTING":
    case "STOPPING":
      return "warn";
    case "FAILED":
    case "CRASHED":
      return "bad";
    case "BLOCKED":
      return "blocked";
    default:
      return "idle";
  }
}

export function projectTone(status: ProjectStatus): Tone {
  switch (status) {
    case "HEALTHY":
      return "ok";
    case "STARTING":
    case "STOPPING":
    case "DEGRADED":
      return status === "DEGRADED" ? "warn" : "info";
    case "FAILED":
      return "bad";
    default:
      return "idle";
  }
}

export function healthTone(status: HealthStatus): Tone {
  switch (status) {
    case "HEALTHY":
      return "ok";
    case "UNHEALTHY":
      return "bad";
    case "CHECKING":
      return "warn";
    default:
      return "idle";
  }
}

export function portTone(status: PortStatus): Tone {
  switch (status) {
    case "BOUND":
      return "ok";
    case "RESERVED":
      return "info";
    case "UNVERIFIED":
      return "warn";
    case "CONFLICT":
      return "bad";
    default:
      return "idle";
  }
}

/** transitional statuses animate, because they are expected to change on their own. */
export function isTransitional(status: ProcessStatus): boolean {
  return status === "STARTING" || status === "STOPPING";
}

export function uptime(seconds: number | undefined, t: Translate): string {
  if (!seconds || seconds < 0) return "—";
  const days = Math.floor(seconds / 86400);
  const hours = Math.floor((seconds % 86400) / 3600);
  const minutes = Math.floor((seconds % 3600) / 60);
  const rest = Math.floor(seconds % 60);
  const unit = (value: number, key: MessageKey) => `${value}${t(key)}`;
  if (days > 0) return `${unit(days, "unit.day")} ${unit(hours, "unit.hour")}`;
  if (hours > 0) return `${unit(hours, "unit.hour")} ${unit(minutes, "unit.minute")}`;
  if (minutes > 0) return `${unit(minutes, "unit.minute")} ${unit(rest, "unit.second")}`;
  return unit(rest, "unit.second");
}

export function clockTime(iso: string | undefined): string {
  if (!iso) return "—";
  const when = new Date(iso);
  if (Number.isNaN(when.getTime())) return "—";
  return when.toLocaleTimeString([], { hour12: false }) + "." + String(when.getMilliseconds()).padStart(3, "0");
}

export function dateTime(iso: string | undefined): string {
  if (!iso) return "—";
  const when = new Date(iso);
  if (Number.isNaN(when.getTime())) return "—";
  return when.toLocaleString([], { hour12: false });
}

/** portsLabel renders a service's ports the way the CLI does: 5173→BOUND. */
export function portsLabel(service: Service): string {
  if (!service.ports || service.ports.length === 0) return "—";
  return service.ports.map((port) => `${port.port}`).join(", ");
}

export function serviceLabel(service: Service): string {
  return service.display_name ? `${service.name} · ${service.display_name}` : service.name;
}

export function projectLabel(name: string, displayName?: string): string {
  return displayName && displayName !== name ? displayName : name;
}

/** captureWarning names the message explaining a service whose output DevMan can
 *  no longer see. The caller translates it. */
export function captureWarning(service: Service): MessageKey | null {
  if (service.observability.log_capture !== "detached") return null;
  return service.observability.adopted ? "svc.captureAdopted" : "svc.captureLost";
}

/** The next four map a daemon token to the key of its one-line gloss. The token
 *  itself is always shown; the gloss goes in a tooltip, so the words DevMan's CLI
 *  and JSON use stay the words on screen. */
export function processStatusHint(status: ProcessStatus): MessageKey {
  return `status.${status}` as MessageKey;
}

export function healthHint(status: HealthStatus): MessageKey {
  return `health.${status}` as MessageKey;
}

export function projectStatusHint(status: ProjectStatus): MessageKey {
  return `project.status.${status}` as MessageKey;
}

export function portStatusHint(status: PortStatus): MessageKey {
  return `port.${status}` as MessageKey;
}
