// Data-loading hooks.
//
// DevMan state changes because a process changed, not because the window did, so
// the pattern here is: load once, then re-load when the daemon says something
// happened. Polling is the fallback for the few things that have no event
// (uptime, port bindings), never the primary mechanism.

import { useCallback, useEffect, useRef, useState } from "react";

import type { Api } from "./api/client";
import { errorCode, errorMessage } from "./api/client";
import { useApi } from "./api/context";
import type { DaemonEvent, LogRecord } from "./api/types";

export interface Resource<T> {
  data: T | null;
  error: string | null;
  loading: boolean;
  reload: () => void;
}

/**
 * useResource loads a value and reloads it when `signal` changes.
 *
 * The loaded value is kept while a reload is in flight: a dashboard that blanks
 * out every time an event arrives is unreadable, and events arrive constantly
 * while services boot.
 */
export function useResource<T>(
  load: (api: Api) => Promise<T>,
  signal: string,
  intervalMs = 0,
): Resource<T> {
  const api = useApi();
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [nonce, setNonce] = useState(0);
  const loader = useRef(load);
  loader.current = load;

  const reload = useCallback(() => setNonce((value) => value + 1), []);

  useEffect(() => {
    let live = true;
    setLoading(true);
    loader
      .current(api)
      .then((value) => {
        if (!live) return;
        setData(value);
        setError(null);
      })
      .catch((cause: unknown) => {
        if (!live) return;
        setError(errorMessage(cause));
      })
      .finally(() => {
        if (live) setLoading(false);
      });
    return () => {
      live = false;
    };
  }, [api, signal, nonce]);

  useEffect(() => {
    if (intervalMs <= 0) return;
    const timer = setInterval(reload, intervalMs);
    return () => clearInterval(timer);
  }, [intervalMs, reload]);

  return { data, error, loading, reload };
}

/** useEventFeed keeps a rolling window of daemon events over one SSE connection. */
export function useEventFeed(limit = 200): { events: DaemonEvent[]; connected: boolean } {
  const api = useApi();
  const [events, setEvents] = useState<DaemonEvent[]>([]);
  const [connected, setConnected] = useState(false);

  useEffect(() => {
    const source = new EventSource(api.eventStreamUrl(40));
    source.addEventListener("open", () => setConnected(true));
    source.addEventListener("error", () => setConnected(false));
    source.addEventListener("event", (message) => {
      const event = parse<DaemonEvent>(message);
      if (!event) return;
      setConnected(true);
      setEvents((current) => append(current, event, limit, (item) => item.seq));
    });
    return () => source.close();
  }, [api, limit]);

  return { events, connected };
}

/** useLogStream follows one service's output, replaying recent history first. */
export function useLogStream(
  projectID: string,
  service: string,
  enabled: boolean,
  limit = 2000,
): { lines: LogRecord[]; connected: boolean; clear: () => void } {
  const api = useApi();
  const [lines, setLines] = useState<LogRecord[]>([]);
  const [connected, setConnected] = useState(false);

  useEffect(() => {
    setLines([]);
    if (!enabled || !projectID || !service) return;
    const source = new EventSource(api.logStreamUrl(projectID, service, 400));
    source.addEventListener("open", () => setConnected(true));
    source.addEventListener("error", () => setConnected(false));
    source.addEventListener("log", (message) => {
      const record = parse<LogRecord>(message);
      if (!record) return;
      setConnected(true);
      setLines((current) => append(current, record, limit, (item) => item.seq));
    });
    return () => {
      source.close();
      setConnected(false);
    };
  }, [api, projectID, service, enabled, limit]);

  return { lines, connected, clear: useCallback(() => setLines([]), []) };
}

/** Outcome is what a caller learns immediately, without waiting for a re-render:
 *  whether the call succeeded and, if not, the failure itself. */
export interface Outcome {
  ok: boolean;
  cause: unknown;
}

export interface Action {
  run: (task: (api: Api) => Promise<unknown>, label?: string) => Promise<Outcome>;
  pending: string | null;
  error: string | null;
  code: string;
  clear: () => void;
}

/**
 * useAction runs one lifecycle call at a time and keeps its failure visible.
 *
 * Start and stop are the calls most likely to be refused (an untrusted project, a
 * port conflict, a missing tool), so the error is state a page can render rather
 * than something logged to a console the user never opens.
 */
export function useAction(): Action {
  const api = useApi();
  const [pending, setPending] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [code, setCode] = useState("");

  const run = useCallback(
    async (task: (client: Api) => Promise<unknown>, label = "working"): Promise<Outcome> => {
      setPending(label);
      setError(null);
      setCode("");
      try {
        await task(api);
        return { ok: true, cause: null };
      } catch (failure: unknown) {
        setError(errorMessage(failure));
        setCode(errorCode(failure));
        return { ok: false, cause: failure };
      } finally {
        setPending(null);
      }
    },
    [api],
  );

  return {
    run,
    pending,
    error,
    code,
    clear: useCallback(() => {
      setError(null);
      setCode("");
    }, []),
  };
}

/** useTick re-renders on an interval, for uptime counters that have no event. */
export function useTick(intervalMs: number): number {
  const [tick, setTick] = useState(0);
  useEffect(() => {
    const timer = setInterval(() => setTick((value) => value + 1), intervalMs);
    return () => clearInterval(timer);
  }, [intervalMs]);
  return tick;
}

function parse<T>(message: Event): T | null {
  const data = (message as MessageEvent<string>).data;
  if (!data) return null;
  try {
    return JSON.parse(data) as T;
  } catch {
    return null;
  }
}

/** append adds a record, dropping a replayed duplicate and trimming the window. */
function append<T>(current: T[], item: T, limit: number, key: (value: T) => number): T[] {
  const last = current.at(-1);
  if (last && key(last) >= key(item)) return current;
  const next = current.length >= limit ? current.slice(current.length - limit + 1) : current.slice();
  next.push(item);
  return next;
}
