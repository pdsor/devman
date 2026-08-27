// Settings: the daemon's global configuration, edited one key at a time.
//
// The daemon revalidates the whole file on every write and refuses an edit it
// could not start from, so a bad value is reported here rather than persisted.

import { useState } from "react";

import { useAction, useResource } from "../hooks";
import { forgetManualEndpoint, inTauri } from "../api/bridge";
import { useApi } from "../api/context";
import { Button, Notice, Page, Panel } from "../ui";

export function SettingsPage() {
  const api = useApi();
  const settings = useResource((client) => client.settings(), "settings");
  const [drafts, setDrafts] = useState<Record<string, string>>({});
  const save = useAction();
  const [savedKey, setSavedKey] = useState("");

  const entries = Object.entries(settings.data ?? {}).sort(([left], [right]) => left.localeCompare(right));

  const commit = async (key: string, value: string) => {
    setSavedKey("");
    const outcome = await save.run((client) => client.setSetting(key, value), `saving ${key}`);
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
    <Page title="Settings" lede={`Applies to every project on this machine. Stored in the daemon's config.yaml.`}>
      {settings.error ? <Notice tone="bad" title="Cannot read settings">{settings.error}</Notice> : null}
      {save.error ? (
        <Notice tone="bad" title={save.code || "The setting was not saved"} actions={<Button small variant="quiet" onClick={save.clear}>Dismiss</Button>}>
          {save.error}
        </Notice>
      ) : null}
      {savedKey ? <Notice tone="ok" title={`Saved ${savedKey}`}>Some settings only affect services started from now on.</Notice> : null}

      <Panel title="Values" padded={false}>
        <table>
          <thead>
            <tr>
              <th style={{ width: "34%" }}>Key</th>
              <th>Value</th>
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
                      Save
                    </Button>
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </Panel>

      <Panel title="This window">
        <p className="muted" style={{ marginTop: 0 }}>
          Connected to <code className="mono">{api.endpoint.base_url}</code>
          {api.endpoint.pid ? ` (daemon pid ${api.endpoint.pid})` : ""}.
        </p>
        <div className="row wrap">
          <Button
            variant="danger"
            onClick={async () => {
              if ((await save.run((client) => client.shutdownDaemon(), "stopping the daemon")).ok) {
                window.location.reload();
              }
            }}
            disabled={save.pending !== null}
            title="Stops every service, then the daemon"
          >
            Stop all services and shut down the daemon
          </Button>
          {!inTauri() ? (
            <Button
              variant="quiet"
              onClick={() => {
                forgetManualEndpoint();
                window.location.reload();
              }}
            >
              Forget this address
            </Button>
          ) : null}
        </div>
      </Panel>
    </Page>
  );
}
