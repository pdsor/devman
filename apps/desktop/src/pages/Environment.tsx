// Environment status: what DevMan can reach, and where it keeps its state.
//
// This page exists because of one specific failure: a GUI launched from a desktop
// icon inherits a different PATH than a shell does, so a tool that works in the
// terminal is missing here. The daemon answers from its own process, which is the
// answer that matters.

import { useFeedSignal } from "../feed";
import { useResource } from "../hooks";
import { dateTime, uptime } from "../format";
import { Fact, Facts, Notice, Page, Panel } from "../ui";

export function EnvironmentPage() {
  const signal = useFeedSignal((event) => event.type === "DAEMON_READY");
  const tools = useResource((api) => api.tools(), signal);
  const paths = useResource((api) => api.paths(), signal);
  const status = useResource((api) => api.daemonStatus(), signal, 10000);

  const missing = (tools.data ?? []).filter((tool) => !tool.found);

  return (
    <Page
      title="Environment"
      lede="Resolved by the daemon, in the daemon's own process — the same lookup a service start uses."
    >
      {status.data && !status.data.info.graceful_signals ? (
        <Notice tone="warn" title="Stops are force kills on this daemon">
          The daemon has no console attached, so it cannot send a graceful interrupt. Services will be terminated
          rather than asked to exit. Starting the daemon from a terminal restores graceful stops.
        </Notice>
      ) : null}

      {status.data ? (
        <Panel padded={false}>
          <div className="panel-head">
            <h2>Daemon</h2>
          </div>
          <Facts>
            <Fact label="Version">{status.data.info.version || "dev"}</Fact>
            <Fact label="API">{status.data.info.api_version}</Fact>
            <Fact label="Address">{status.data.info.host}:{status.data.info.port}</Fact>
            <Fact label="PID">{status.data.info.pid}</Fact>
            <Fact label="Uptime">{uptime(status.data.uptime_seconds)}</Fact>
            <Fact label="Started">{dateTime(status.data.info.started_at)}</Fact>
            <Fact label="Projects">{status.data.projects}</Fact>
            <Fact label="Running services">{status.data.running_services}</Fact>
          </Facts>
        </Panel>
      ) : null}

      <Panel
        title="Tools"
        aside={
          missing.length > 0 ? (
            <span className="mono t-warn">{missing.length} not found</span>
          ) : (
            <span className="mono t-ok">all found</span>
          )
        }
        padded={false}
      >
        <table>
          <thead>
            <tr>
              <th>Tool</th>
              <th>Resolved to</th>
            </tr>
          </thead>
          <tbody>
            {(tools.data ?? []).map((tool) => (
              <tr key={tool.name}>
                <td className={tool.found ? "" : "t-warn"}>{tool.name}</td>
                <td className={tool.found ? "" : "faint"}>{tool.path || "not on PATH"}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </Panel>

      {paths.data ? (
        <Panel padded={false}>
          <div className="panel-head">
            <h2>Files</h2>
          </div>
          <Facts>
            <Fact label="Data directory">{paths.data.home}</Fact>
            <Fact label="Settings">{paths.data.settings}</Fact>
            <Fact label="Database">{paths.data.database}</Fact>
            <Fact label="Discovery">{paths.data.daemon}</Fact>
            <Fact label="Auth token">{paths.data.auth_token}</Fact>
            <Fact label="Logs">{paths.data.logs}</Fact>
          </Facts>
        </Panel>
      ) : null}

      {tools.error ? <Notice tone="bad" title="Cannot probe tools">{tools.error}</Notice> : null}
    </Page>
  );
}
