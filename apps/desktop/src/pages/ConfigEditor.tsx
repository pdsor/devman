// devman.yaml editor.
//
// This edits the project's real file — the same one a user edits by hand — through
// the daemon, which validates before writing. There is no second representation of
// a project's configuration, because two representations eventually disagree.

import { useEffect, useState } from "react";

import { ApiError } from "../api/client";
import { useAction, useResource } from "../hooks";
import { useNav } from "../nav";
import { Button, Notice, Page, Panel } from "../ui";
import type { ApiErrorBody, ValidationResult } from "../api/types";

export function ConfigPage(props: { id: string }) {
  const navigate = useNav();
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
    }, "saving");
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
      title="Configuration"
      lede={document.data?.path ?? "devman.yaml"}
      actions={
        <>
          <Button variant="primary" disabled={!dirty || save.pending !== null} onClick={() => void submit()}>
            {save.pending ? "Saving…" : "Save"}
          </Button>
          <Button variant="quiet" disabled={!dirty} onClick={() => setDraft(null)}>
            Revert
          </Button>
          <Button variant="quiet" onClick={() => navigate({ page: "project", id: props.id })}>
            Back to services
          </Button>
        </>
      }
    >
      {document.error ? <Notice tone="bad" title="Cannot read the configuration">{document.error}</Notice> : null}

      {save.error && save.code !== "CONFIG_INVALID" ? (
        <Notice tone="bad" title={save.code || "The save failed"}>{save.error}</Notice>
      ) : null}

      {refused ? (
        <Notice tone="bad" title="Not saved — the configuration is invalid">
          The file on disk is unchanged. Fix the findings below and save again.
        </Notice>
      ) : null}

      {saved && project.data && !project.data.trusted ? (
        <Notice
          tone="warn"
          title="Saved, and this project needs approval again"
          actions={
            <Button
              small
              onClick={async () => {
                if ((await save.run((api) => api.trust(props.id), "approving")).ok) project.reload();
              }}
            >
              Approve
            </Button>
          }
        >
          The edit changed what the project executes, so DevMan is asking again before it runs anything.
        </Notice>
      ) : null}

      {saved && project.data?.trusted ? <Notice tone="ok" title="Saved">Restart the services to pick up the change.</Notice> : null}

      <Panel padded={false}>
        <div className="panel-head">
          <h2>devman.yaml</h2>
          <div className="spacer" />
          {dirty ? <span className="mono t-warn">unsaved changes</span> : <span className="mono faint">in sync with disk</span>}
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
  const validation = props.validation;
  if (!validation) return null;
  const errors = validation.errors ?? [];
  const warnings = validation.warnings ?? [];
  if (errors.length === 0 && warnings.length === 0) {
    return (
      <p className="mono t-ok" style={{ marginTop: 12 }}>
        valid
      </p>
    );
  }
  return (
    <Panel title="Findings" padded={false}>
      <table>
        <thead>
          <tr>
            <th>Kind</th>
            <th>Where</th>
            <th>What</th>
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
  return (
    <tr>
      <td className={props.kind === "error" ? "t-bad" : "t-warn"}>{props.issue.code}</td>
      <td>{props.issue.path || "—"}</td>
      <td>{props.issue.message}</td>
    </tr>
  );
}
