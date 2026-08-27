// Port viewer: every allocation DevMan holds, plus a lookup for "who has this
// port", which also answers for processes DevMan does not manage.

import { useState } from "react";

import { useFeedSignal } from "../feed";
import { useAction, useResource } from "../hooks";
import { dateTime, portStatusHint, portTone } from "../format";
import { useT } from "../i18n";
import { useNav } from "../nav";
import { Button, Chip, Empty, Notice, Page, Panel } from "../ui";
import type { PortUsage } from "../api/types";

export function PortsPage(props: { port?: number }) {
  const navigate = useNav();
  const t = useT();
  const signal = useFeedSignal((event) => event.type.startsWith("PORT_"));
  const allocations = useResource((api) => api.ports(), signal, 8000);
  const [query, setQuery] = useState(props.port ? String(props.port) : "");
  const [usage, setUsage] = useState<PortUsage | null>(null);
  const lookup = useAction();

  const check = async () => {
    const port = Number(query);
    if (!Number.isInteger(port) || port < 1 || port > 65535) return;
    setUsage(null);
    await lookup.run(async (api) => {
      setUsage(await api.portUsage(port));
    }, t("action.checking", { port }));
  };

  return (
    <Page
      title={t("ports.title")}
      lede={t("ports.lede")}
      actions={
        <Button variant="quiet" onClick={allocations.reload} disabled={allocations.loading}>
          {t("common.refresh")}
        </Button>
      }
    >
      {allocations.error ? <Notice tone="bad" title={t("ports.listFailed")}>{allocations.error}</Notice> : null}

      <Panel padded={false}>
        <div className="panel-head">
          <h2>{t("ports.whoTitle")}</h2>
          <div className="spacer" />
          <input
            className="input mono"
            style={{ width: 120 }}
            inputMode="numeric"
            placeholder="3000"
            value={query}
            onChange={(event) => setQuery(event.target.value.replace(/[^\d]/g, ""))}
            onKeyDown={(event) => {
              if (event.key === "Enter") void check();
            }}
          />
          <Button small onClick={() => void check()} disabled={lookup.pending !== null || query === ""}>
            {t("ports.check")}
          </Button>
        </div>
        <div className="panel-body">
          {lookup.error ? <span className="mono t-bad">{lookup.error}</span> : null}
          {!lookup.error && !usage ? <span className="muted">{t("ports.hint")}</span> : null}
          {usage ? <UsageAnswer usage={usage} /> : null}
        </div>
      </Panel>

      <Panel title={t("ports.allocations")} padded={false}>
        {allocations.data && allocations.data.length === 0 ? (
          <Empty>{t("ports.empty")}</Empty>
        ) : (
          <table>
            <thead>
              <tr>
                <th className="num">{t("ports.col.port")}</th>
                <th>{t("ports.col.status")}</th>
                <th>{t("ports.col.project")}</th>
                <th>{t("ports.col.service")}</th>
                <th>{t("ports.col.name")}</th>
                <th>{t("ports.col.env")}</th>
                <th>{t("ports.col.allocated")}</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {(allocations.data ?? []).map((allocation) => (
                <tr key={`${allocation.project}/${allocation.service}/${allocation.port}`}>
                  <td className="num">{allocation.port}</td>
                  <td>
                    <Chip
                      tone={portTone(allocation.status)}
                      label={allocation.status}
                      title={t(portStatusHint(allocation.status))}
                    />
                  </td>
                  <td>{allocation.project}</td>
                  <td>{allocation.service}</td>
                  <td>{allocation.name || "—"}</td>
                  <td>{allocation.env || "—"}</td>
                  <td>{dateTime(allocation.allocated_at)}</td>
                  <td className="actions">
                    <Button small variant="quiet" onClick={() => navigate({ page: "project", id: allocation.project })}>
                      {t("ports.openProject")}
                    </Button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Panel>
    </Page>
  );
}

function UsageAnswer(props: { usage: PortUsage }) {
  const t = useT();
  const usage = props.usage;
  if (!usage.occupied && !usage.allocation) {
    return <span className="mono t-ok">{t("ports.free", { port: usage.port })}</span>;
  }
  if (usage.allocation) {
    return (
      <span className="mono">
        {t("ports.owned", {
          port: usage.port,
          project: usage.allocation.project,
          service: usage.allocation.service,
          status: usage.allocation.status.toLowerCase(),
        })}
      </span>
    );
  }
  const owner = usage.owner?.pid
    ? `（pid ${usage.owner.pid}${usage.owner.name ? `, ${usage.owner.name}` : ""}）`
    : "";
  return <span className="mono t-warn">{t("ports.foreign", { port: usage.port, owner })}</span>;
}
