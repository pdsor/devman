// Settings: the daemon's global configuration, edited one key at a time, plus the
// two settings that belong to this window rather than to DevMan.
//
// The daemon revalidates the whole file on every write and refuses an edit it
// could not start from, so a bad value is reported here rather than persisted.

import { useState } from "react";

import { useAction, useResource } from "../hooks";
import { forgetManualEndpoint, inTauri } from "../api/bridge";
import { useApi } from "../api/context";
import { useT } from "../i18n";
import { Button, LanguageChoice, Notice, Page, Panel } from "../ui";

export function SettingsPage() {
  const api = useApi();
  const t = useT();
  const settings = useResource((client) => client.settings(), "settings");
  const [drafts, setDrafts] = useState<Record<string, string>>({});
  const save = useAction();
  const [savedKey, setSavedKey] = useState("");

  const entries = Object.entries(settings.data ?? {}).sort(([left], [right]) => left.localeCompare(right));

  const commit = async (key: string, value: string) => {
    setSavedKey("");
    const outcome = await save.run((client) => client.setSetting(key, value), t("action.savingKey", { key }));
    if (!outcome.ok) return;
    setSavedKey(key);
    setDrafts((current) => {
      const next = { ...current };
      delete next[key];
      return next;
    });
    settings.reload();
  };

  return (
    <Page title={t("settings.title")} lede={t("settings.lede")}>
      {settings.error ? <Notice tone="bad" title={t("settings.readFailed")}>{settings.error}</Notice> : null}
      {save.error ? (
        <Notice
          tone="bad"
          title={save.code || t("settings.saveFailed")}
          actions={<Button small variant="quiet" onClick={save.clear}>{t("common.dismiss")}</Button>}
        >
          {save.error}
        </Notice>
      ) : null}
      {savedKey ? (
        <Notice tone="ok" title={t("settings.savedKey", { key: savedKey })}>{t("settings.savedBody")}</Notice>
      ) : null}

      <Panel title={t("settings.language")}>
        <p className="muted" style={{ marginTop: 0 }}>
          {t("settings.languageBody")}
        </p>
        <LanguageChoice />
      </Panel>

      <Panel title={t("settings.values")} padded={false}>
        <table>
          <thead>
            <tr>
              <th style={{ width: "34%" }}>{t("settings.col.key")}</th>
              <th>{t("settings.col.value")}</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {entries.map(([key, value]) => {
              const draft = drafts[key];
              const current = draft ?? value;
              const dirty = draft !== undefined && draft !== value;
              return (
                <tr key={key}>
                  <td>{key}</td>
                  <td>
                    <input
                      className="input mono"
                      value={current}
                      onChange={(event) => setDrafts({ ...drafts, [key]: event.target.value })}
                      onKeyDown={(event) => {
                        if (event.key === "Enter") void commit(key, current);
                      }}
                    />
                  </td>
                  <td className="actions">
                    <Button
                      small
                      variant={dirty ? "primary" : "quiet"}
                      disabled={!dirty || save.pending !== null}
                      onClick={() => void commit(key, current)}
                    >
                      {t("common.save")}
                    </Button>
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </Panel>

      <Panel title={t("settings.window")}>
        <p className="muted mono" style={{ marginTop: 0 }}>
          {api.endpoint.pid
            ? t("settings.connectedToPid", { url: api.endpoint.base_url, pid: api.endpoint.pid })
            : t("settings.connectedTo", { url: api.endpoint.base_url })}
        </p>
        <div className="row wrap">
          <Button
            variant="danger"
            onClick={async () => {
              if ((await save.run((client) => client.shutdownDaemon(), t("action.shuttingDown"))).ok) {
                window.location.reload();
              }
            }}
            disabled={save.pending !== null}
            title={t("settings.shutdownHint")}
          >
            {t("settings.shutdown")}
          </Button>
          {!inTauri() ? (
            <Button
              variant="quiet"
              onClick={() => {
                forgetManualEndpoint();
                window.location.reload();
              }}
            >
              {t("settings.forget")}
            </Button>
          ) : null}
        </div>
      </Panel>
    </Page>
  );
}
