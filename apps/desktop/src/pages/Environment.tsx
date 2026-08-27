// Environment status: what DevMan can reach, and where it keeps its state.
//
// This page exists because of one specific failure: a GUI launched from a desktop
// icon inherits a different PATH than a shell does, so a tool that works in the
// terminal is missing here. The daemon answers from its own process, which is the
// answer that matters.

import { useFeedSignal } from "../feed";
import { useResource } from "../hooks";
import { dateTime, uptime } from "../format";
import { useT } from "../i18n";
import { Fact, Facts, Notice, Page, Panel } from "../ui";

export function EnvironmentPage() {
  const t = useT();
  const signal = useFeedSignal((event) => event.type === "DAEMON_READY");
  const tools = useResource((api) => api.tools(), signal);
  const paths = useResource((api) => api.paths(), signal);
  const status = useResource((api) => api.daemonStatus(), signal, 10000);

  const missing = (tools.data ?? []).filter((tool) => !tool.found);

  return (
    <Page title={t("env.title")} lede={t("env.lede")}>
      {status.data && !status.data.info.graceful_signals ? (
        <Notice tone="warn" title={t("env.forceKillTitle")}>{t("env.forceKillBody")}</Notice>
      ) : null}

      {status.data ? (
        <Panel padded={false}>
          <div className="panel-head">
            <h2>{t("env.daemon")}</h2>
          </div>
          <Facts>
            <Fact label={t("env.version")}>{status.data.info.version || "dev"}</Fact>
            <Fact label={t("env.api")}>{status.data.info.api_version}</Fact>
            <Fact label={t("env.address")}>
              {status.data.info.host}:{status.data.info.port}
            </Fact>
            <Fact label={t("env.pid")}>{status.data.info.pid}</Fact>
            <Fact label={t("env.uptime")}>{uptime(status.data.uptime_seconds, t)}</Fact>
            <Fact label={t("env.started")}>{dateTime(status.data.info.started_at)}</Fact>
            <Fact label={t("env.projects")}>{status.data.projects}</Fact>
            <Fact label={t("env.runningServices")}>{status.data.running_services}</Fact>
          </Facts>
        </Panel>
      ) : null}

      <Panel
        title={t("env.tools")}
        aside={
          missing.length > 0 ? (
            <span className="mono t-warn">{t("env.missing", { count: missing.length })}</span>
          ) : (
            <span className="mono t-ok">{t("env.allFound")}</span>
          )
        }
        padded={false}
      >
        <table>
          <thead>
            <tr>
              <th>{t("env.col.tool")}</th>
              <th>{t("env.col.resolved")}</th>
            </tr>
          </thead>
          <tbody>
            {(tools.data ?? []).map((tool) => (
              <tr key={tool.name}>
                <td className={tool.found ? "" : "t-warn"}>{tool.name}</td>
                <td className={tool.found ? "" : "faint"}>{tool.path || t("env.notOnPath")}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </Panel>

      {paths.data ? (
        <Panel padded={false}>
          <div className="panel-head">
            <h2>{t("env.files")}</h2>
          </div>
          <Facts>
            <Fact label={t("env.dataDir")}>{paths.data.home}</Fact>
            <Fact label={t("env.settings")}>{paths.data.settings}</Fact>
            <Fact label={t("env.database")}>{paths.data.database}</Fact>
            <Fact label={t("env.discovery")}>{paths.data.daemon}</Fact>
            <Fact label={t("env.authToken")}>{paths.data.auth_token}</Fact>
            <Fact label={t("env.logs")}>{paths.data.logs}</Fact>
          </Facts>
        </Panel>
      ) : null}

      {tools.error ? <Notice tone="bad" title={t("env.probeFailed")}>{tools.error}</Notice> : null}
    </Page>
  );
}
