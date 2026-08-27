// Project registration, including the trust decision.
//
// Registration is two steps on purpose: DevMan first shows exactly what the
// project would execute, and only then offers to approve it. The approval is the
// user's, never the interface's default.

import { useState } from "react";

import { inTauri, pickDirectory } from "../api/bridge";
import { useAction, useResource } from "../hooks";
import { useT } from "../i18n";
import { useNav } from "../nav";
import { Button, Empty, Notice, Page, Panel } from "../ui";
import type { Preview } from "../api/types";

export function RegisterPage() {
  const navigate = useNav();
  const t = useT();
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
    }, t("action.reading"));
  };

  const browse = async () => {
    const chosen = await pickDirectory(t("register.folder"));
    if (!chosen) return;
    setPath(chosen);
    await runInspect(chosen);
  };

  const submit = async () => {
    if (!preview) return;
    const created = await register.run(async (api) => {
      await api.register(preview.path, approve);
    }, t("action.registering"));
    if (created.ok) navigate({ page: "projects" });
  };

  const invalid = preview?.validation && preview.validation.valid === false;
  const issues = (list: { path?: string; message: string }[] | null | undefined) =>
    (list ?? []).map((issue) => `${issue.path ? `${issue.path}: ` : ""}${issue.message}`).join(" · ");

  return (
    <Page title={t("register.title")} lede={t("register.lede")}>
      <Panel title={t("register.folder")}>
        <div className="row wrap">
          <input
            className="input mono"
            style={{ flex: 1, minWidth: 260 }}
            placeholder={t("register.placeholder")}
            value={path}
            onChange={(event) => setPath(event.target.value)}
            onKeyDown={(event) => {
              if (event.key === "Enter" && path) void runInspect(path);
            }}
          />
          {inTauri() ? <Button onClick={() => void browse()}>{t("register.browse")}</Button> : null}
          <Button variant="primary" disabled={!path || inspect.pending !== null} onClick={() => void runInspect(path)}>
            {t("register.read")}
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
            <Notice tone="info" title={t("register.already")}>{t("register.alreadyBody")}</Notice>
          ) : null}

          {invalid ? (
            <Notice tone="bad" title={t("register.invalidTitle")}>{issues(preview.validation?.errors)}</Notice>
          ) : null}

          {(preview.validation?.warnings ?? []).length > 0 ? (
            <Notice tone="warn" title={t("register.warningsTitle")}>{issues(preview.validation?.warnings)}</Notice>
          ) : null}

          <Panel title={t("register.previewTitle", { name: preview.name })} padded={false}>
            {preview.execution.length === 0 ? (
              <Empty>{t("register.noServices")}</Empty>
            ) : (
              <table>
                <thead>
                  <tr>
                    <th>{t("register.col.service")}</th>
                    <th>{t("register.col.runtime")}</th>
                    <th>{t("register.col.command")}</th>
                    <th>{t("register.col.cwd")}</th>
                    <th>{t("register.col.envFiles")}</th>
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

          <Panel title={t("register.approvalTitle")}>
            <p className="muted" style={{ marginTop: 0 }}>
              {t("register.approvalBody")}
            </p>
            <label className="row" style={{ gap: 8 }}>
              <input type="checkbox" checked={approve} onChange={(event) => setApprove(event.target.checked)} />
              <span>{t("register.approvalCheckbox")}</span>
            </label>
            <div className="row" style={{ marginTop: 14 }}>
              <Button
                variant="primary"
                disabled={register.pending !== null || invalid === true}
                onClick={() => void submit()}
              >
                {approve ? t("register.addApprove") : t("register.addNoApprove")}
              </Button>
              <Button variant="quiet" onClick={() => setPreview(null)}>
                {t("common.cancel")}
              </Button>
              <span className="faint mono">
                {t("register.fingerprint", { value: preview.execution_fingerprint.slice(0, 12) })}
              </span>
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
        <Panel title={t("register.knownTitle")} padded={false}>
          <table>
            <thead>
              <tr>
                <th>{t("register.col.project")}</th>
                <th>{t("register.col.path")}</th>
                <th>{t("register.col.trust")}</th>
              </tr>
            </thead>
            <tbody>
              {existing.data.map((project) => (
                <tr key={project.id}>
                  <td>{project.name}</td>
                  <td>{project.path}</td>
                  <td className={project.trusted ? "t-ok" : "t-blocked"}>
                    {project.trusted ? t("project.trustApproved") : t("project.trustNotApproved")}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </Panel>
      ) : null}
    </Page>
  );
}
