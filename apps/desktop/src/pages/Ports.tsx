// Port viewer: every allocation DevMan holds, plus a lookup for "who has this
// port", which also answers for processes DevMan does not manage.

import { useState } from "react";

import { useFeedSignal } from "../feed";
import { useAction, useResource } from "../hooks";
import { dateTime, portTone } from "../format";
import { useNav } from "../nav";
import { Button, Chip, Empty, Notice, Page, Panel } from "../ui";
import type { PortUsage } from "../api/types";

export function PortsPage(props: { port?: number }) {
  const navigate = useNav();
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
    }, `checking ${port}`);
  };

  return (
    <Page
      title="Ports"
      lede="Allocations are made before a process starts, so two projects never race for the same port."
      actions={<Button variant="quiet" onClick={allocations.reload} disabled={allocations.loading}>Refresh</Button>}
    >
      {allocations.error ? <Notice tone="bad" title="Cannot list ports">{allocations.error}</Notice> : null}

      <Panel padded={false}>
        <div className="panel-head">
          <h2>Who has a port</h2>
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
            Check
          </Button>
        </div>
        <div className="panel-body">
          {lookup.error ? <span className="mono t-bad">{lookup.error}</span> : null}
          {!lookup.error && !usage ? <span className="muted">Enter a port number to see who holds it.</span> : null}
          {usage ? <UsageAnswer usage={usage} /> : null}
        </div>
      </Panel>

      <Panel title="Allocations" padded={false}>
        {allocations.data && allocations.data.length === 0 ? (
          <Empty>No ports are allocated. DevMan reserves them when a service starts.</Empty>
        ) : (
          <table>
            <thead>
              <tr>
                <th className="num">Port</th>
                <th>Status</th>
                <th>Project</th>
                <th>Service</th>
                <th>Name</th>
                <th>Env</th>
                <th>Allocated</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {(allocations.data ?? []).map((allocation) => (
                <tr key={`${allocation.project}/${allocation.service}/${allocation.port}`}>
                  <td className="num">{allocation.port}</td>
                  <td>
                    <Chip tone={portTone(allocation.status)} label={allocation.status} />
                  </td>
                  <td>{allocation.project}</td>
                  <td>{allocation.service}</td>
                  <td>{allocation.name || "—"}</td>
                  <td>{allocation.env || "—"}</td>
                  <td>{dateTime(allocation.allocated_at)}</td>
                  <td className="actions">
                    <Button small variant="quiet" onClick={() => navigate({ page: "project", id: allocation.project })}>
                      Open project
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
  const usage = props.usage;
  if (!usage.occupied && !usage.allocation) {
    return (
      <span className="mono t-ok">
        {usage.port} is free.
      </span>
    );
  }
  if (usage.allocation) {
    return (
      <span className="mono">
        {usage.port} belongs to{" "}
        <b>
          {usage.allocation.project}/{usage.allocation.service}
        </b>{" "}
        ({usage.allocation.status.toLowerCase()}).
      </span>
    );
  }
  return (
    <span className="mono t-warn">
      {usage.port} is held by a process DevMan does not manage
      {usage.owner?.pid ? ` (pid ${usage.owner.pid}${usage.owner.name ? `, ${usage.owner.name}` : ""})` : ""}.
    </span>
  );
}
