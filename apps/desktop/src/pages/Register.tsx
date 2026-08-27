// Project registration, including the trust decision.
//
// Registration is two steps on purpose: DevMan first shows exactly what the
// project would execute, and only then offers to approve it. The approval is the
// user's, never the interface's default.

import { useState } from "react";

import { inTauri, pickDirectory } from "../api/bridge";
import { useAction, useResource } from "../hooks";
import { useNav } from "../nav";
import { Button, Empty, Notice, Page, Panel } from "../ui";
import type { Preview } from "../api/types";

export function RegisterPage() {
  const navigate = useNav();
  const [path, setPath] = useState("");
  const [preview, setPreview] = useState<Preview | null>(null);
  const [approve, setApprove] = useState(false);
  const inspect = useAction();
  const register = useAction();
  // Shown so a user can tell a duplicate registration from a new one.
  const existing = useResource((api) => api.projects(false), "register");

  const runInspect = async (target: string) => {
    setPreview(null);
    setApprove(false);
    await inspect.run(async (api) => {
      setPreview(await api.inspect(target));
    }, "reading the project");
  };

  const browse = async () => {
    const chosen = await pickDirectory("Choose a project folder");
    if (!chosen) return;
    setPath(chosen);
    await runInspect(chosen);
  };

  const submit = async () => {
    if (!preview) return;
    const created = await register.run(async (api) => {
      await api.register(preview.path, approve);
    }, "registering");
    if (created.ok) navigate({ page: "projects" });
  };

  const invalid = preview?.validation && preview.validation.valid === false;

  return (
    <Page
      title="Add a project"
      lede="Point DevMan at a folder containing devman.yaml. Nothing runs until you approve what it executes."
    >
      <Panel title="Project folder">
        <div className="row wrap">
          <input
            className="input mono"
            style={{ flex: 1, minWidth: 260 }}
            placeholder={inTauri() ? "C:\\code\\my-app" : "/path/to/project"}
            value={path}
            onChange={(event) => setPath(event.target.value)}
            onKeyDown={(event) => {
              if (event.key === "Enter" && path) void runInspect(path);
            }}
          />
          {inTauri() ? (
            <Button onClick={() => void browse()}>Browse…</Button>
          ) : null}
          <Button variant="primary" disabled={!path || inspect.pending !== null} onClick={() => void runInspect(path)}>
            Read it
          </Button>
        </div>
        {inspect.error ? (
          <p className="mono t-bad" style={{ marginBottom: 0 }}>
            {inspect.code ? `${inspect.code}: ` : ""}
            {inspect.error}
          </p>
        ) : null}
      </Panel>

      {preview ? (
        <>
          {preview.already_registered ? (
            <Notice tone="info" title="Already registered">
              Registering it again updates the record and re-reads the configuration.
            </Notice>
          ) : null}

          {invalid ? (
            <Notice tone="bad" title="This configuration will not load">
              {(preview.validation?.errors ?? []).map((issue) => `${issue.path ? `${issue.path}: ` : ""}${issue.message}`).join(" · ")}
            </Notice>
          ) : null}

          {(preview.validation?.warnings ?? []).length > 0 ? (
            <Notice tone="warn" title="Warnings">
              {(preview.validation?.warnings ?? []).map((issue) => `${issue.path ? `${issue.path}: ` : ""}${issue.message}`).join(" · ")}
            </Notice>
          ) : null}

          <Panel title={`${preview.name} — what it would run`} padded={false}>
            {preview.execution.length === 0 ? (
              <Empty>This configuration declares no services.</Empty>
            ) : (
              <table>
                <thead>
                  <tr>
                    <th>Service</th>
                    <th>Runtime</th>
                    <th>Command</th>
                    <th>Working directory</th>
                    <th>Env files</th>
                  </tr>
                </thead>
                <tbody>
                  {preview.execution.map((item) => (
                    <tr key={item.service}>
                      <td>{item.service}</td>
                      <td>{item.runtime}</td>
                      <td>{item.command_line || item.shell || item.compose || "—"}</td>
                      <td>{item.cwd}</td>
                      <td>{item.env_files && item.env_files.length > 0 ? item.env_files.join(", ") : "—"}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </Panel>

          <Panel title="Approval">
            <p className="muted" style={{ marginTop: 0 }}>
              Approving lets DevMan run the commands above. If a command, working directory or env file changes later,
              DevMan asks again — cosmetic edits do not.
            </p>
            <label className="row" style={{ gap: 8 }}>
              <input type="checkbox" checked={approve} onChange={(event) => setApprove(event.target.checked)} />
              <span>I have read the commands and approve running them</span>
            </label>
            <div className="row" style={{ marginTop: 14 }}>
              <Button variant="primary" disabled={register.pending !== null || invalid === true} onClick={() => void submit()}>
                {approve ? "Add and approve" : "Add without approving"}
              </Button>
              <Button variant="quiet" onClick={() => setPreview(null)}>
                Cancel
              </Button>
              <span className="faint mono">fingerprint {preview.execution_fingerprint.slice(0, 12)}</span>
            </div>
            {register.error ? (
              <p className="mono t-bad">
                {register.code ? `${register.code}: ` : ""}
                {register.error}
              </p>
            ) : null}
          </Panel>
        </>
      ) : null}

      {existing.data && existing.data.length > 0 && !preview ? (
        <Panel title="Already known" padded={false}>
          <table>
            <thead>
              <tr>
                <th>Project</th>
                <th>Path</th>
                <th>Trust</th>
              </tr>
            </thead>
            <tbody>
              {existing.data.map((project) => (
                <tr key={project.id}>
                  <td>{project.name}</td>
                  <td>{project.path}</td>
                  <td className={project.trusted ? "t-ok" : "t-blocked"}>{project.trusted ? "approved" : "not approved"}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </Panel>
      ) : null}
    </Page>
  );
}
