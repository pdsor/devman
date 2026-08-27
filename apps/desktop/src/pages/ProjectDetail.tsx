// Service dashboard: one project's services, with the state DevMan has for each
// and the controls that change it.

import { useFeed, useFeedSignal } from "../feed";
import { useAction, useResource, useTick } from "../hooks";
import {
  captureWarning,
  dateTime,
  healthTone,
  isTransitional,
  processTone,
  projectLabel,
  projectTone,
  serviceLabel,
  uptime,
} from "../format";
import { useNav } from "../nav";
import { openExternal } from "../api/bridge";
import { Button, Chip, Empty, Fact, Facts, Notice, Page, Panel, Strip } from "../ui";
import type { Service } from "../api/types";

export function ProjectPage(props: { id: string }) {
  const navigate = useNav();
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
      <Page title="Project" actions={<Button onClick={() => navigate({ page: "projects" })} variant="quiet">Back</Button>}>
        <Notice tone="bad" title="Cannot load this project">{project.error}</Notice>
      </Page>
    );
  }

  const data = project.data;
  if (!data) {
    return (
      <Page title="Project">
        <Panel><Empty>Loading…</Empty></Panel>
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
            title={data.trusted ? undefined : "Approve the project first"}
            onClick={() => run((api) => api.startProject(data.id, { all: true }), "starting all services")}
          >
            Start all
          </Button>
          <Button
            disabled={action.pending !== null || running === 0}
            onClick={() => run((api) => api.stopProject(data.id, { all: true }), "stopping all services")}
          >
            Stop all
          </Button>
          <Button
            disabled={action.pending !== null || !data.trusted}
            onClick={() => run((api) => api.restartProject(data.id, { all: true }), "restarting all services")}
          >
            Restart all
          </Button>
          <Button variant="quiet" onClick={() => navigate({ page: "config", id: data.id })}>
            Edit devman.yaml
          </Button>
        </>
      }
    >
      {action.error ? (
        <Notice
          tone="bad"
          title={action.code || "That did not work"}
          actions={<Button onClick={action.clear} variant="quiet" small>Dismiss</Button>}
        >
          {action.error}
        </Notice>
      ) : null}

      {data.config_error ? (
        <Notice
          tone="bad"
          title={`devman.yaml is not usable (${data.config_error.code})`}
          actions={<Button small onClick={() => navigate({ page: "config", id: data.id })}>Open the editor</Button>}
        >
          {data.config_error.message}
          {data.config_error.path ? ` — at ${data.config_error.path}` : ""}
        </Notice>
      ) : null}

      {!data.trusted && !data.config_error ? (
        <Notice
          tone="warn"
          title="This project needs your approval"
          actions={
            <Button small disabled={action.pending !== null} onClick={() => run((api) => api.trust(data.id), "approving")}>
              Approve
            </Button>
          }
        >
          DevMan will not run commands from a configuration you have not seen. Approval covers what this project
          executes; a later edit to a command, working directory or env file asks again.
        </Notice>
      ) : null}

      <Panel padded={false}>
        <div className="panel-head">
          <h2>State</h2>
          <div className="spacer" />
          <Chip tone={projectTone(data.status)} label={data.status} pulsing={data.status === "STARTING"} />
        </div>
        <Facts>
          <Fact label="Services">{running}/{services.length} running</Fact>
          <Fact label="Healthy">{data.summary.healthy}</Fact>
          <Fact label="Trust">{data.trusted ? "approved" : "not approved"}</Fact>
          <Fact label="Last started">{dateTime(data.last_started_at)}</Fact>
          <Fact label="Config">{data.config_path}</Fact>
        </Facts>
      </Panel>

      <Panel
        title="Services"
        aside={<span className="mono faint">newest event on the right</span>}
        padded={false}
      >
        {services.length === 0 ? (
          <Empty>This project declares no services.</Empty>
        ) : (
          services.map((service) => (
            <ServiceRow
              key={service.name}
              service={service}
              busy={action.pending !== null}
              trusted={data.trusted}
              events={events.filter((event) => event.project === data.id && event.service === service.name)}
              onStart={() => run((api) => api.startService(data.id, service.name), `starting ${service.name}`)}
              onStop={() => run((api) => api.stopService(data.id, service.name), `stopping ${service.name}`)}
              onRestart={() => run((api) => api.restartService(data.id, service.name), `restarting ${service.name}`)}
              onLogs={() => navigate({ page: "logs", id: data.id, service: service.name })}
            />
          ))
        )}
      </Panel>

      <div className="row" style={{ marginTop: 14 }}>
        <Button
          variant="danger"
          disabled={action.pending !== null || running > 0}
          title={running > 0 ? "Stop the services first" : "Remove this project from DevMan"}
          onClick={async () => {
            if ((await action.run((api) => api.unregister(data.id), "removing the project")).ok) {
              navigate({ page: "projects" });
            }
          }}
        >
          Remove project
        </Button>
        {data.trusted ? (
          <Button
            variant="quiet"
            disabled={action.pending !== null}
            onClick={() => run((api) => api.trust(data.id, true), "revoking approval")}
          >
            Revoke approval
          </Button>
        ) : null}
      </div>
    </Page>
  );
}

function ServiceRow(props: {
  service: Service;
  events: Parameters<typeof Strip>[0]["events"];
  busy: boolean;
  trusted: boolean;
  onStart: () => void;
  onStop: () => void;
  onRestart: () => void;
  onLogs: () => void;
}) {
  const service = props.service;
  const capture = captureWarning(service);
  const stopped = service.status === "STOPPED" || service.status === "FAILED" || service.status === "CRASHED" || service.status === "BLOCKED";

  return (
    <div className="svc">
      <div>
        <div className="svc-name">
          <Chip tone={processTone(service.status)} label={service.status} pulsing={isTransitional(service.status)} />
          <strong>{serviceLabel(service)}</strong>
          {service.health.status !== "N/A" ? (
            <Chip tone={healthTone(service.health.status)} label={service.health.status} />
          ) : null}
          {service.observability.adopted ? <span className="mono faint">adopted</span> : null}
          {service.desired_state === "STOPPED" && service.status === "STOPPED" ? (
            <span className="mono faint">kept stopped</span>
          ) : null}
        </div>

        <div className="svc-meta">
          <span>{service.runtime}</span>
          {service.pid ? <span>pid {service.pid}</span> : null}
          {service.uptime_seconds ? <span>up {uptime(service.uptime_seconds)}</span> : null}
          {service.restart_count > 0 ? <span>{service.restart_count} restarts</span> : null}
          {service.last_exit_code !== undefined && service.status !== "RUNNING" ? (
            <span>exit {service.last_exit_code}</span>
          ) : null}
          {service.ports && service.ports.length > 0 ? (
            <span>
              {service.ports.map((port) => `${port.port} ${port.status.toLowerCase()}`).join(" · ")}
            </span>
          ) : null}
          {service.depends_on && service.depends_on.length > 0 ? (
            <span>after {service.depends_on.join(", ")}</span>
          ) : null}
        </div>

        {service.url ? (
          <div className="svc-meta">
            <button type="button" className="link mono" onClick={() => void openExternal(service.url ?? "")}>
              {service.url}
            </button>
          </div>
        ) : null}

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

        {capture ? <div className="svc-meta t-warn">{capture}</div> : null}

        <div style={{ marginTop: 8 }}>
          <Strip events={props.events} ticks={40} />
        </div>
      </div>

      <div className="svc-controls">
        {stopped ? (
          <Button small variant="primary" disabled={props.busy || !props.trusted} onClick={props.onStart}>
            Start
          </Button>
        ) : (
          <Button small disabled={props.busy} onClick={props.onStop}>
            Stop
          </Button>
        )}
        <Button small disabled={props.busy || !props.trusted} onClick={props.onRestart}>
          Restart
        </Button>
        <Button small variant="quiet" onClick={props.onLogs}>
          Logs
        </Button>
      </div>
    </div>
  );
}
