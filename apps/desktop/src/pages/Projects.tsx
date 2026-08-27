// Project dashboard: every registered project, what it is doing, and the two
// actions a developer takes most often.

import { useFeed, useFeedSignal } from "../feed";
import { useAction, useResource } from "../hooks";
import { projectLabel, projectTone } from "../format";
import { useNav } from "../nav";
import { Button, Chip, Empty, Notice, Page, Panel, Strip } from "../ui";
import type { Project } from "../api/types";

export function ProjectsPage() {
  const navigate = useNav();
  const signal = useFeedSignal();
  const { events } = useFeed();
  const projects = useResource((api) => api.projects(true), signal);
  const action = useAction();

  const run = async (task: Parameters<typeof action.run>[0], label: string) => {
    if ((await action.run(task, label)).ok) projects.reload();
  };

  return (
    <Page
      title="Projects"
      lede="DevMan owns these processes. Closing a terminal, or this window, does not stop them."
      actions={
        <>
          <Button onClick={() => navigate({ page: "register" })} variant="primary">
            Add a project
          </Button>
          <Button onClick={projects.reload} variant="quiet" disabled={projects.loading}>
            Refresh
          </Button>
        </>
      }
    >
      {projects.error ? <Notice tone="bad" title="Cannot list projects">{projects.error}</Notice> : null}
      {action.error ? (
        <Notice tone="bad" title="That did not work" actions={<Button onClick={action.clear} variant="quiet" small>Dismiss</Button>}>
          {action.error}
        </Notice>
      ) : null}

      {projects.data && projects.data.length === 0 ? (
        <Panel>
          <Empty>
            No projects yet. Add one and DevMan reads its <code className="mono">devman.yaml</code> to learn what to run.
          </Empty>
        </Panel>
      ) : null}

      <div className="cards">
        {(projects.data ?? []).map((project) => (
          <article className="card" key={project.id}>
            <div className="row">
              <h3>{projectLabel(project.name, project.display_name)}</h3>
              <div style={{ marginLeft: "auto" }}>
                <Chip
                  tone={projectTone(project.status)}
                  label={project.status}
                  pulsing={project.status === "STARTING" || project.status === "STOPPING"}
                />
              </div>
            </div>
            <div className="card-path">{project.path}</div>

            <Strip events={events.filter((event) => event.project === project.id)} />

            <div className="tally">
              <span>
                <b>{project.summary.running}</b>/{project.summary.total} running
              </span>
              <span>
                <b>{project.summary.healthy}</b> healthy
              </span>
              {project.summary.failed > 0 ? (
                <span className="t-bad">
                  <b>{project.summary.failed}</b> failed
                </span>
              ) : null}
            </div>

            {project.config_error ? (
              <div className="mono t-bad">
                {project.config_error.code}: {project.config_error.message}
              </div>
            ) : null}
            {!project.trusted && !project.config_error ? (
              <div className="mono t-blocked">not trusted — approve it before starting</div>
            ) : null}

            <div className="row wrap">
              <Button small onClick={() => navigate({ page: "project", id: project.id })}>
                Open
              </Button>
              <Button
                small
                variant="primary"
                disabled={action.pending !== null || !project.trusted}
                title={project.trusted ? "Start every service in dependency order" : "Approve the project first"}
                onClick={() => run((api) => api.startProject(project.id, { all: true }), `starting ${project.name}`)}
              >
                Start all
              </Button>
              <Button
                small
                disabled={action.pending !== null || project.summary.running === 0}
                onClick={() => run((api) => api.stopProject(project.id, { all: true }), `stopping ${project.name}`)}
              >
                Stop all
              </Button>
              {!project.trusted ? (
                <Button
                  small
                  disabled={action.pending !== null}
                  onClick={() => run((api) => api.trust(project.id), `approving ${project.name}`)}
                >
                  Approve
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
  if (props.projects.length === 0) return null;
  const services = props.projects.reduce((total, project) => total + project.summary.total, 0);
  const running = props.projects.reduce((total, project) => total + project.summary.running, 0);
  return (
    <p className="muted mono" style={{ marginTop: 16 }}>
      {props.projects.length} projects · {running}/{services} services running
    </p>
  );
}
