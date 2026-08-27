// devman.yaml editor.
//
// This edits the project's real file — the same one a user edits by hand — through
// the daemon, which validates before writing. There is no second representation of
// a project's configuration, because two representations eventually disagree.

import { useEffect, useState } from "react";

import { ApiError } from "../api/client";
import { useAction, useResource } from "../hooks";
import { useT } from "../i18n";
import { useNav } from "../nav";
import { Button, Notice, Page, Panel } from "../ui";
import type { ApiErrorBody, ValidationResult } from "../api/types";

export function ConfigPage(props: { id: string }) {
  const navigate = useNav();
  const t = useT();
  const document = useResource((api) => api.configFile(props.id), props.id);
  const project = useResource((api) => api.project(props.id), props.id);
  const [draft, setDraft] = useState<string | null>(null);
  const [refused, setRefused] = useState<ValidationResult | null>(null);
  const [saved, setSaved] = useState(false);
  const save = useAction();

  useEffect(() => {
    setDraft(null);
    setRefused(null);
    setSaved(false);
  }, [props.id]);

  const content = draft ?? document.data?.content ?? "";
  const dirty = draft !== null && draft !== document.data?.content;

  const submit = async () => {
    setRefused(null);
    setSaved(false);
    const outcome = await save.run(async (api) => {
      await api.saveConfigFile(props.id, content);
    }, t("action.saving"));
    if (outcome.ok) {
      setDraft(null);
      setSaved(true);
      document.reload();
      project.reload();
      return;
    }
    // The daemon attaches its findings to a CONFIG_INVALID refusal so they can be
    // shown next to the text that caused them.
    setRefused(outcome.cause instanceof ApiError ? outcome.cause.validation() : null);
  };

  const validation = refused ?? document.data?.validation ?? null;

  return (
    <Page
      title={t("config.title")}
      lede={document.data?.path ?? "devman.yaml"}
      actions={
        <>
          <Button variant="primary" disabled={!dirty || save.pending !== null} onClick={() => void submit()}>
            {save.pending ? t("config.saving") : t("config.save")}
          </Button>
          <Button variant="quiet" disabled={!dirty} onClick={() => setDraft(null)}>
            {t("config.revert")}
          </Button>
          <Button variant="quiet" onClick={() => navigate({ page: "project", id: props.id })}>
            {t("config.back")}
          </Button>
        </>
      }
    >
      {document.error ? <Notice tone="bad" title={t("config.readFailed")}>{document.error}</Notice> : null}

      {/* A YAML parse error arrives without structured findings, so the message
          itself has to be shown or the refusal would be silent. */}
      {save.error && !refused ? (
        <Notice tone="bad" title={save.code || t("config.saveFailed")}>{save.error}</Notice>
      ) : null}

      {refused ? (
        <Notice tone="bad" title={t("config.refusedTitle")}>{t("config.refusedBody")}</Notice>
      ) : null}

      {saved && project.data && !project.data.trusted ? (
        <Notice
          tone="warn"
          title={t("config.savedNeedsTrustTitle")}
          actions={
            <Button
              small
              onClick={async () => {
                if ((await save.run((api) => api.trust(props.id), t("action.approving"))).ok) project.reload();
              }}
            >
              {t("config.approve")}
            </Button>
          }
        >
          {t("config.savedNeedsTrustBody")}
        </Notice>
      ) : null}

      {saved && project.data?.trusted ? (
        <Notice tone="ok" title={t("config.savedTitle")}>{t("config.savedBody")}</Notice>
      ) : null}

      <Panel padded={false}>
        <div className="panel-head">
          <h2>devman.yaml</h2>
          <div className="spacer" />
          {dirty ? (
            <span className="mono t-warn">{t("config.dirty")}</span>
          ) : (
            <span className="mono faint">{t("config.inSync")}</span>
          )}
        </div>
        <div className="panel-body">
          <textarea
            className="textarea"
            spellCheck={false}
            value={content}
            onChange={(event) => setDraft(event.target.value)}
          />
        </div>
      </Panel>

      <Findings validation={validation} />
    </Page>
  );
}

function Findings(props: { validation: ValidationResult | null }) {
  const t = useT();
  const validation = props.validation;
  if (!validation) return null;
  const errors = validation.errors ?? [];
  const warnings = validation.warnings ?? [];
  if (errors.length === 0 && warnings.length === 0) {
    return (
      <p className="mono t-ok" style={{ marginTop: 12 }}>
        {t("config.valid")}
      </p>
    );
  }
  return (
    <Panel title={t("config.findings")} padded={false}>
      <table>
        <thead>
          <tr>
            <th>{t("config.col.kind")}</th>
            <th>{t("config.col.where")}</th>
            <th>{t("config.col.what")}</th>
          </tr>
        </thead>
        <tbody>
          {errors.map((issue, index) => (
            <Row key={`e${index}`} kind="error" issue={issue} />
          ))}
          {warnings.map((issue, index) => (
            <Row key={`w${index}`} kind="warning" issue={issue} />
          ))}
        </tbody>
      </table>
    </Panel>
  );
}

function Row(props: { kind: "error" | "warning"; issue: ApiErrorBody }) {
  const t = useT();
  return (
    <tr>
      <td className={props.kind === "error" ? "t-bad" : "t-warn"} title={t(props.kind === "error" ? "config.error" : "config.warning")}>
        {props.issue.code}
      </td>
      <td>{props.issue.path || "—"}</td>
      <td>{props.issue.message}</td>
    </tr>
  );
}
