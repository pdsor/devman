// The window: connect to a daemon, then show the panel.
//
// Connecting is a first-class state rather than an error page. A developer opening
// DevMan after a reboot has no daemon running, and the honest response is to offer
// to start one, not to show a failed request.

import { useCallback, useEffect, useMemo, useState } from "react";

import { Api } from "./api/client";
import { ApiContext } from "./api/context";
import {
  forgetManualEndpoint,
  inTauri,
  rememberManualEndpoint,
  resolveEndpoint,
  startDaemon,
  type Endpoint,
} from "./api/bridge";
import { FeedContext } from "./feed";
import { useEventFeed, useResource } from "./hooks";
import { uptime } from "./format";
import { NavContext, type Route } from "./nav";
import { ConfigPage } from "./pages/ConfigEditor";
import { EnvironmentPage } from "./pages/Environment";
import { EventsPage } from "./pages/Events";
import { LogsPage } from "./pages/Logs";
import { PortsPage } from "./pages/Ports";
import { ProjectPage } from "./pages/ProjectDetail";
import { ProjectsPage } from "./pages/Projects";
import { RegisterPage } from "./pages/Register";
import { SettingsPage } from "./pages/Settings";
import { Button, Chip } from "./ui";

export function App() {
  const [endpoint, setEndpoint] = useState<Endpoint | null>(null);
  const [problem, setProblem] = useState<string | null>(null);
  const [busy, setBusy] = useState(true);

  const connect = useCallback(async () => {
    setBusy(true);
    setProblem(null);
    try {
      setEndpoint(await resolveEndpoint());
    } catch (cause: unknown) {
      setProblem(cause instanceof Error ? cause.message : String(cause));
    } finally {
      setBusy(false);
    }
  }, []);

  useEffect(() => {
    void connect();
  }, [connect]);

  const api = useMemo(() => (endpoint ? new Api(endpoint) : null), [endpoint]);

  if (!api || !endpoint) {
    return (
      <Connect
        busy={busy}
        problem={problem}
        onRetry={() => void connect()}
        onStart={async () => {
          setBusy(true);
          setProblem(null);
          try {
            setEndpoint(await startDaemon());
          } catch (cause: unknown) {
            setProblem(cause instanceof Error ? cause.message : String(cause));
          } finally {
            setBusy(false);
          }
        }}
        onManual={(baseUrl, token) => setEndpoint(rememberManualEndpoint(baseUrl, token))}
      />
    );
  }

  return (
    <ApiContext.Provider value={api}>
      <Workspace
        endpoint={endpoint}
        onDisconnect={() => {
          if (!inTauri()) forgetManualEndpoint();
          setEndpoint(null);
          void connect();
        }}
      />
    </ApiContext.Provider>
  );
}

function Workspace(props: { endpoint: Endpoint; onDisconnect: () => void }) {
  const [route, setRoute] = useState<Route>({ page: "projects" });
  const feed = useEventFeed(400);
  const status = useResource((api) => api.daemonStatus(), feed.connected ? "connected" : "offline", 10000);

  const navigate = useCallback((next: Route) => setRoute(next), []);

  return (
    <NavContext.Provider value={navigate}>
      <FeedContext.Provider value={feed}>
        <div className="shell">
          <nav className="rail">
            <div className="brand">
              <strong>DevMan</strong>
              <span>{props.endpoint.version || "dev"}</span>
            </div>

            <div className="nav">
              <div className="nav-group">Run</div>
              <NavItem label="Projects" active={route.page === "projects" || route.page === "project"} onClick={() => navigate({ page: "projects" })} />
              <NavItem label="Add a project" active={route.page === "register"} onClick={() => navigate({ page: "register" })} />

              <div className="nav-group">Inspect</div>
              <NavItem label="Logs" active={route.page === "logs"} onClick={() => navigate({ page: "logs" })} />
              <NavItem label="Ports" active={route.page === "ports"} onClick={() => navigate({ page: "ports" })} />
              <NavItem label="Activity" active={route.page === "events"} onClick={() => navigate({ page: "events" })} />

              <div className="nav-group">Machine</div>
              <NavItem label="Environment" active={route.page === "environment"} onClick={() => navigate({ page: "environment" })} />
              <NavItem label="Settings" active={route.page === "settings"} onClick={() => navigate({ page: "settings" })} />
            </div>

            <div className="rail-foot">
              <span>{props.endpoint.host}:{props.endpoint.port}</span>
              <span>api {props.endpoint.api_version}</span>
            </div>
          </nav>

          <div className="main">
            <header className="topbar">
              <Chip
                tone={feed.connected ? "ok" : "warn"}
                label={feed.connected ? "DAEMON LIVE" : "RECONNECTING"}
                pulsing={!feed.connected}
              />
              {status.data ? (
                <>
                  <span className="topbar-fact">up {uptime(status.data.uptime_seconds)}</span>
                  <span className="topbar-fact">
                    {status.data.running_services} running · {status.data.projects} projects
                  </span>
                </>
              ) : null}
              <div className="spacer" />
              <Button variant="quiet" small onClick={props.onDisconnect}>
                Reconnect
              </Button>
            </header>

            <main className="content">
              <Screen route={route} />
            </main>
          </div>
        </div>
      </FeedContext.Provider>
    </NavContext.Provider>
  );
}

function Screen(props: { route: Route }) {
  const route = props.route;
  switch (route.page) {
    case "projects":
      return <ProjectsPage />;
    case "project":
      return <ProjectPage id={route.id} />;
    case "logs":
      return <LogsPage projectID={route.id} service={route.service} />;
    case "ports":
      return <PortsPage port={route.port} />;
    case "register":
      return <RegisterPage />;
    case "config":
      return <ConfigPage id={route.id} />;
    case "events":
      return <EventsPage />;
    case "environment":
      return <EnvironmentPage />;
    case "settings":
      return <SettingsPage />;
  }
}

function NavItem(props: { label: string; active: boolean; onClick: () => void }) {
  return (
    <button type="button" className="nav-item" aria-current={props.active ? "page" : undefined} onClick={props.onClick}>
      {props.label}
    </button>
  );
}

function Connect(props: {
  busy: boolean;
  problem: string | null;
  onRetry: () => void;
  onStart: () => void;
  onManual: (baseUrl: string, token: string) => void;
}) {
  const [baseUrl, setBaseUrl] = useState("http://127.0.0.1:8765/api/v1");
  const [token, setToken] = useState("");

  return (
    <div className="connect">
      <div className="connect-card">
        <h1>DevMan</h1>
        <p>
          {props.busy
            ? "Looking for the daemon…"
            : props.problem
              ? props.problem
              : "Connected."}
        </p>

        {inTauri() ? (
          <div className="row">
            <Button variant="primary" onClick={props.onStart} disabled={props.busy}>
              Start the daemon
            </Button>
            <Button variant="quiet" onClick={props.onRetry} disabled={props.busy}>
              Look again
            </Button>
          </div>
        ) : (
          <>
            <p className="muted">
              This page is running in a browser, so it cannot start the daemon or read its token. Run{" "}
              <code className="mono">devman daemon start</code>, then paste the address and token from{" "}
              <code className="mono">devman paths</code>.
            </p>
            <label className="label" htmlFor="base">API address</label>
            <input id="base" className="input" value={baseUrl} onChange={(event) => setBaseUrl(event.target.value)} />
            <div style={{ height: 10 }} />
            <label className="label" htmlFor="token">Auth token</label>
            <input id="token" className="input" value={token} onChange={(event) => setToken(event.target.value)} />
            <div className="row">
              <Button variant="primary" onClick={() => props.onManual(baseUrl, token)} disabled={!baseUrl || !token}>
                Connect
              </Button>
              <Button variant="quiet" onClick={props.onRetry}>
                Look again
              </Button>
            </div>
          </>
        )}
      </div>
    </div>
  );
}
