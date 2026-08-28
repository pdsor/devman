// Service dashboard: one project's services, with the state DevMan has for each
// and the controls that change it.

import { useState } from "react";
import { useFeed, useFeedSignal } from "../feed";
import { useAction, useResource, useTick } from "../hooks";
import {
  captureWarning,
  dateTime,
  healthHint,
  healthTone,
  isTransitional,
  processStatusHint,
  processTone,
  projectLabel,
  projectStatusHint,
  projectTone,
  serviceLabel,
  uptime,
} from "../format";
import { useT, type Translate } from "../i18n";
import { useNav } from "../nav";
import { openExternal } from "../api/bridge";
import { Button, Chip, Empty, Fact, Facts, Notice, Page, Panel, Strip } from "../ui";
import type { Service } from "../api/types";

export function ProjectPage(props: { id: string }) {
  const navigate = useNav();
  const t = useT();
  const signal = useFeedSignal((event) => event.project === props.id);
  const { events } = useFeed();
  // Uptime and port bindings have no event of their own, so this one view polls
  // slowly on top of the event-driven reload.
  const project = useResource((api) => api.project(props.id), `${props.id}:${signal}`, 5000);
  const action = useAction();
  useTick(1000);

  const run = async (task: Parameters<typeof action.run>[0], label: string) => {
    if ((await action.run(task, label)).ok) project.reload();
  };

  if (project.error && !project.data) {
    return (
      <Page
        title={t("project.title")}
        actions={
          <Button onClick={() => navigate({ page: "projects" })} variant="quiet">
            {t("common.back")}
          </Button>
        }
      >
        <Notice tone="bad" title={t("project.loadFailed")}>{project.error}</Notice>
      </Page>
    );
  }

  const data = project.data;
  if (!data) {
    return (
      <Page title={t("project.title")}>
        <Panel><Empty>{t("common.loading")}</Empty></Panel>
      </Page>
    );
  }

  const services = data.services ?? [];
  const running = services.filter((service) => service.status === "RUNNING").length;

  return (
    <Page
      title={projectLabel(data.name, data.display_name)}
      lede={data.path}
      actions={
        <>
          <Button
            variant="primary"
            disabled={action.pending !== null || !data.trusted}
            title={data.trusted ? undefined : t("project.approveFirst")}
            onClick={() => run((api) => api.startProject(data.id, { all: true }), t("action.startingAll"))}
          >
            {t("project.startAll")}
          </Button>
          <Button
            disabled={action.pending !== null || running === 0}
            onClick={() => run((api) => api.stopProject(data.id, { all: true }), t("action.stoppingAll"))}
          >
            {t("project.stopAll")}
          </Button>
          <Button
            disabled={action.pending !== null || !data.trusted}
            onClick={() => run((api) => api.restartProject(data.id, { all: true }), t("action.restartingAll"))}
          >
            {t("project.restartAll")}
          </Button>
          <Button variant="quiet" onClick={() => navigate({ page: "config", id: data.id })}>
            {t("project.editConfig")}
          </Button>
        </>
      }
    >
      {action.error ? (
        <Notice
          tone="bad"
          title={action.code || t("projects.actionFailed")}
          actions={<Button onClick={action.clear} variant="quiet" small>{t("common.dismiss")}</Button>}
        >
          {action.error}
        </Notice>
      ) : null}

      {data.config_error ? (
        <Notice
          tone="bad"
          title={t("project.configBroken", { code: data.config_error.code })}
          actions={
            <Button small onClick={() => navigate({ page: "config", id: data.id })}>
              {t("project.openEditor")}
            </Button>
          }
        >
          {data.config_error.message}
          {data.config_error.path ? ` — ${data.config_error.path}` : ""}
        </Notice>
      ) : null}

      {!data.trusted && !data.config_error ? (
        <Notice
          tone="warn"
          title={t("project.trustTitle")}
          actions={
            <Button small disabled={action.pending !== null} onClick={() => run((api) => api.trust(data.id), t("action.approving"))}>
              {t("project.approve")}
            </Button>
          }
        >
          {t("project.trustBody")}
        </Notice>
      ) : null}

      <Panel padded={false}>
        <div className="panel-head">
          <h2>{t("project.state")}</h2>
          <div className="spacer" />
          <Chip
            tone={projectTone(data.status)}
            label={data.status}
            title={t(projectStatusHint(data.status))}
            pulsing={data.status === "STARTING"}
          />
        </div>
        <Facts>
          <Fact label={t("project.services")}>{t("project.servicesCount", { running, total: services.length })}</Fact>
          <Fact label={t("project.healthy")}>{data.summary.healthy}</Fact>
          <Fact label={t("project.trust")}>
            {data.trusted ? t("project.trustApproved") : t("project.trustNotApproved")}
          </Fact>
          <Fact label={t("project.lastStarted")}>{dateTime(data.last_started_at)}</Fact>
          <Fact label={t("project.config")}>{data.config_path}</Fact>
        </Facts>
      </Panel>

      <Panel
        title={t("project.services")}
        aside={<span className="mono faint">{t("project.stripHint")}</span>}
        padded={false}
      >
        {services.length === 0 ? (
          <Empty>{t("project.noServices")}</Empty>
        ) : (
          services.map((service) => (
            <ServiceRow
              key={service.name}
              service={service}
              t={t}
              busy={action.pending !== null}
              trusted={data.trusted}
              events={events.filter((event) => event.project === data.id && event.service === service.name)}
              onStart={() => run((api) => api.startService(data.id, service.name), t("action.starting", { name: service.name }))}
              onStop={() => run((api) => api.stopService(data.id, service.name), t("action.stopping", { name: service.name }))}
              onRestart={() =>
                run((api) => api.restartService(data.id, service.name), t("action.restarting", { name: service.name }))
              }
              onLogs={() => navigate({ page: "logs", id: data.id, service: service.name })}
            />
          ))
        )}
      </Panel>

      <div className="row" style={{ marginTop: 14 }}>
        <Button
          variant="danger"
          disabled={action.pending !== null || running > 0}
          title={running > 0 ? t("project.removeHintRunning") : t("project.removeHint")}
          onClick={async () => {
            if ((await action.run((api) => api.unregister(data.id), t("action.removing"))).ok) {
              navigate({ page: "projects" });
            }
          }}
        >
          {t("project.remove")}
        </Button>
        {data.trusted ? (
          <Button
            variant="quiet"
            disabled={action.pending !== null}
            onClick={() => run((api) => api.trust(data.id, true), t("action.revoking"))}
          >
            {t("project.revoke")}
          </Button>
        ) : null}
      </div>
    </Page>
  );
}

function ServiceRow(props: {
  service: Service;
  events: Parameters<typeof Strip>[0]["events"];
  t: Translate;
  busy: boolean;
  trusted: boolean;
  onStart: () => void;
  onStop: () => void;
  onRestart: () => void;
  onLogs: () => void;
}) {
  const { service, t } = props;
  const [openError, setOpenError] = useState("");
  const capture = captureWarning(service);
  // A denied opener scope or a machine with no browser registered rejects
  // silently otherwise, which looks exactly like a dead button.
  const openURL = async () => {
    if (!service.url) return;
    setOpenError("");
    try {
      await openExternal(service.url);
    } catch (error) {
      setOpenError(t("svc.openFailed", { url: service.url, error: String(error) }));
    }
  };

  const stopped =
    service.status === "STOPPED" ||
    service.status === "FAILED" ||
    service.status === "CRASHED" ||
    service.status === "BLOCKED";

  return (
    <div className="svc">
      <div>
        <div className="svc-name">
          <Chip
            tone={processTone(service.status)}
            label={service.status}
            title={t(processStatusHint(service.status))}
            pulsing={isTransitional(service.status)}
          />
          <strong>{serviceLabel(service)}</strong>
          {service.health.status !== "N/A" ? (
            <Chip
              tone={healthTone(service.health.status)}
              label={service.health.status}
              title={t(healthHint(service.health.status))}
            />
          ) : null}
          {service.observability.adopted ? <span className="mono faint">{t("svc.adopted")}</span> : null}
          {service.desired_state === "STOPPED" && service.status === "STOPPED" ? (
            <span className="mono faint">{t("svc.keptStopped")}</span>
          ) : null}
        </div>

        <div className="svc-meta">
          <span>{service.runtime}</span>
          {service.pid ? <span>{t("svc.pid", { pid: service.pid })}</span> : null}
          {service.uptime_seconds ? <span>{t("svc.up", { value: uptime(service.uptime_seconds, t) })}</span> : null}
          {service.restart_count > 0 ? <span>{t("svc.restarts", { count: service.restart_count })}</span> : null}
          {service.last_exit_code !== undefined && service.status !== "RUNNING" ? (
            <span>{t("svc.exit", { code: service.last_exit_code })}</span>
          ) : null}
          {service.ports && service.ports.length > 0 ? (
            <span>{service.ports.map((port) => `${port.port} ${port.status.toLowerCase()}`).join(" · ")}</span>
          ) : null}
          {service.depends_on && service.depends_on.length > 0 ? (
            <span>{t("svc.after", { names: service.depends_on.join(", ") })}</span>
          ) : null}
        </div>

        {service.url ? (
          <div className="svc-meta">
            <button type="button" className="link mono" onClick={openURL}>
              {service.url}
            </button>
          </div>
        ) : null}

        {openError ? <div className="svc-meta t-bad">{openError}</div> : null}


        {service.command_line ? <div className="svc-meta faint">{service.command_line}</div> : null}

        {service.message ? (
          <div className="svc-meta" style={{ marginTop: 6 }}>
            <span className={service.status === "BLOCKED" ? "t-blocked" : "t-bad"}>
              {service.reason ? `${service.reason.code}: ` : ""}
              {service.message}
            </span>
          </div>
        ) : null}

        {service.health.message && service.health.status === "UNHEALTHY" ? (
          <div className="svc-meta t-bad">{service.health.message}</div>
        ) : null}

        {capture ? <div className="svc-meta t-warn">{t(capture)}</div> : null}

        <div style={{ marginTop: 8 }}>
          <Strip events={props.events} ticks={40} />
        </div>
      </div>

      <div className="svc-controls">
        {stopped ? (
          <Button small variant="primary" disabled={props.busy || !props.trusted} onClick={props.onStart}>
            {t("svc.start")}
          </Button>
        ) : (
          <Button small disabled={props.busy} onClick={props.onStop}>
            {t("svc.stop")}
          </Button>
        )}
        <Button small disabled={props.busy || !props.trusted} onClick={props.onRestart}>
          {t("svc.restart")}
        </Button>
        <Button small variant="quiet" onClick={props.onLogs}>
          {t("svc.logs")}
        </Button>
      </div>
    </div>
  );
}
