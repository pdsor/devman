// Project dashboard: every registered project, what it is doing, and the two
// actions a developer takes most often.

import { useFeed, useFeedSignal } from "../feed";
import { useAction, useResource } from "../hooks";
import { projectLabel, projectStatusHint, projectTone } from "../format";
import { useT } from "../i18n";
import { useNav } from "../nav";
import { Button, Chip, Empty, Notice, Page, Panel, Strip } from "../ui";
import type { Project } from "../api/types";

export function ProjectsPage() {
  const navigate = useNav();
  const t = useT();
  const signal = useFeedSignal();
  const { events } = useFeed();
  const projects = useResource((api) => api.projects(true), signal);
  const action = useAction();

  const run = async (task: Parameters<typeof action.run>[0], label: string) => {
    if ((await action.run(task, label)).ok) projects.reload();
  };

  return (
    <Page
      title={t("projects.title")}
      lede={t("projects.lede")}
      actions={
        <>
          <Button onClick={() => navigate({ page: "register" })} variant="primary">
            {t("projects.add")}
          </Button>
          <Button onClick={projects.reload} variant="quiet" disabled={projects.loading}>
            {t("common.refresh")}
          </Button>
        </>
      }
    >
      {projects.error ? <Notice tone="bad" title={t("projects.listFailed")}>{projects.error}</Notice> : null}
      {action.error ? (
        <Notice
          tone="bad"
          title={action.code || t("projects.actionFailed")}
          actions={<Button onClick={action.clear} variant="quiet" small>{t("common.dismiss")}</Button>}
        >
          {action.error}
        </Notice>
      ) : null}

      {projects.data && projects.data.length === 0 ? (
        <Panel>
          <Empty>{t("projects.empty")}</Empty>
        </Panel>
      ) : null}

      <div className="cards">
        {(projects.data ?? []).map((project) => (
          <article
            className="card"
            key={project.id}
            role="button"
            tabIndex={0}
            aria-label={t("projects.open")}
            onClick={() => navigate({ page: "project", id: project.id })}
            onKeyDown={(event) => {
              if (event.key === "Enter" || event.key === " ") {
                event.preventDefault();
                navigate({ page: "project", id: project.id });
              }
            }}
          >

            <div className="row">
              <h3>{projectLabel(project.name, project.display_name)}</h3>
              <div style={{ marginLeft: "auto" }}>
                <Chip
                  tone={projectTone(project.status)}
                  label={project.status}
                  title={t(projectStatusHint(project.status))}
                  pulsing={project.status === "STARTING" || project.status === "STOPPING"}
                />
              </div>
            </div>
            <div className="card-path">{project.path}</div>

            <Strip events={events.filter((event) => event.project === project.id)} />

            <div className="tally">
              <span>{t("projects.running", { running: project.summary.running, total: project.summary.total })}</span>
              <span>{t("projects.healthy", { count: project.summary.healthy })}</span>
              {project.summary.failed > 0 ? (
                <span className="t-bad">{t("projects.failed", { count: project.summary.failed })}</span>
              ) : null}
            </div>

            {project.config_error ? (
              <div className="mono t-bad">
                {project.config_error.code}: {project.config_error.message}
              </div>
            ) : null}
            {!project.trusted && !project.config_error ? (
              <div className="mono t-blocked">{t("projects.untrusted")}</div>
            ) : null}

            {/* The card itself navigates, so the buttons must not: a click on
                Start should start, not also open the project. */}
            <div className="row wrap" onClick={(event) => event.stopPropagation()}>
              <Button

                small
                variant="primary"
                disabled={action.pending !== null || !project.trusted}
                title={project.trusted ? t("projects.startHintTrusted") : t("projects.startHintUntrusted")}
                onClick={() =>
                  run((api) => api.startProject(project.id, { all: true }), t("action.starting", { name: project.name }))
                }
              >
                {t("projects.startAll")}
              </Button>
              <Button
                small
                disabled={action.pending !== null || project.summary.running === 0}
                onClick={() =>
                  run((api) => api.stopProject(project.id, { all: true }), t("action.stopping", { name: project.name }))
                }
              >
                {t("projects.stopAll")}
              </Button>
              {!project.trusted ? (
                <Button
                  small
                  disabled={action.pending !== null}
                  onClick={() => run((api) => api.trust(project.id), t("action.approving"))}
                >
                  {t("projects.approve")}
                </Button>
              ) : null}
            </div>
          </article>
        ))}
      </div>

      {action.pending ? <p className="muted mono">{action.pending}…</p> : null}
      <Summary projects={projects.data ?? []} />
    </Page>
  );
}

function Summary(props: { projects: Project[] }) {
  const t = useT();
  if (props.projects.length === 0) return null;
  const services = props.projects.reduce((total, project) => total + project.summary.total, 0);
  const running = props.projects.reduce((total, project) => total + project.summary.running, 0);
  return (
    <p className="muted mono" style={{ marginTop: 16 }}>
      {t("projects.summary", { projects: props.projects.length, running, services })}
    </p>
  );
}
