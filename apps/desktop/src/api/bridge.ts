// bridge.ts is the only file that knows it is running inside Tauri.
//
// Two things the webview cannot do for itself live on the Rust side: reading the
// daemon's discovery file and token from disk, and starting the daemon. In a
// plain browser (`pnpm dev` without the shell) the same information can be
// supplied by hand, so the pages work either way.

import type { Paths } from "./types";

export interface Endpoint {
  base_url: string;
  token: string;
  host: string;
  port: number;
  pid: number;
  version: string;
  api_version: string;
}

const MANUAL_KEY = "devman.endpoint";

export function inTauri(): boolean {
  return typeof window !== "undefined" && "__TAURI_INTERNALS__" in window;
}

async function invoke<T>(command: string, args?: Record<string, unknown>): Promise<T> {
  const core = await import("@tauri-apps/api/core");
  return core.invoke<T>(command, args);
}

/** manualEndpoint is the browser-development escape hatch. */
export function manualEndpoint(): Endpoint | null {
  if (typeof localStorage === "undefined") return null;
  const stored = localStorage.getItem(MANUAL_KEY);
  if (!stored) return null;
  try {
    return JSON.parse(stored) as Endpoint;
  } catch {
    return null;
  }
}

export function rememberManualEndpoint(baseUrl: string, token: string): Endpoint {
  const url = new URL(baseUrl);
  const endpoint: Endpoint = {
    base_url: baseUrl.replace(/\/+$/, ""),
    token,
    host: url.hostname,
    port: Number(url.port || 80),
    pid: 0,
    version: "",
    api_version: "v1",
  };
  localStorage.setItem(MANUAL_KEY, JSON.stringify(endpoint));
  return endpoint;
}

export function forgetManualEndpoint(): void {
  localStorage.removeItem(MANUAL_KEY);
}

/** resolveEndpoint finds a running daemon without starting one. */
export async function resolveEndpoint(): Promise<Endpoint> {
  if (inTauri()) return invoke<Endpoint>("daemon_endpoint");
  const manual = manualEndpoint();
  if (manual) return manual;
  // Codes, not sentences: the window renders the failure and the window is the
  // side that knows which language to say it in.
  throw new Error("NO_ENDPOINT");
}

/** startDaemon launches the daemon and waits for it to answer. */
export async function startDaemon(): Promise<Endpoint> {
  if (!inTauri()) {
    throw new Error("NEEDS_DESKTOP");
  }
  return invoke<Endpoint>("start_daemon");
}

/** localPaths reports where DevMan keeps its state, even with the daemon down. */
export async function localPaths(): Promise<Paths | null> {
  if (!inTauri()) return null;
  const layout = await invoke<Paths>("devman_paths");
  return layout;
}

/** pickDirectory opens the native folder picker, used when registering a project. */
export async function pickDirectory(title: string): Promise<string | null> {
  if (!inTauri()) return null;
  const dialog = await import("@tauri-apps/plugin-dialog");
  const chosen = await dialog.open({ directory: true, multiple: false, title });
  return typeof chosen === "string" ? chosen : null;
}

/** openExternal hands a URL to the user's browser rather than the app window. */
export async function openExternal(url: string): Promise<void> {
  if (!inTauri()) {
    window.open(url, "_blank", "noopener,noreferrer");
    return;
  }
  const opener = await import("@tauri-apps/plugin-opener");
  await opener.openUrl(url);
}
