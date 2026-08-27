// Activity: the daemon's event stream, unfiltered.
//
// Every other page shows derived state; this one shows the record it was derived
// from, which is what you read when a service did something you did not expect.

import { useMemo, useState } from "react";

import { useFeed } from "../feed";
import { useResource } from "../hooks";
import { clockTime } from "../format";
import { useNav } from "../nav";
import { Button, Chip, Empty, Page, Panel, Strip } from "../ui";
import type { DaemonEvent } from "../api/types";

export function EventsPage() {
  const navigate = useNav();
  const { events, connected } = useFeed();
  const history = useResource((api) => api.events(200), "events");
  const [needle, setNeedle] = useState("");

  // The stream carries what happened since the window opened; history carries what
  // happened before it. Merged by seq, they read as one log.
  const merged = useMemo(() => {
    const seen = new Map<number, DaemonEvent>();
    for (const event of history.data ?? []) seen.set(event.seq, event);
    for (const event of events) seen.set(event.seq, event);
    return [...seen.values()].sort((left, right) => right.seq - left.seq);
  }, [history.data, events]);

  const visible = useMemo(() => {
    const lowered = needle.toLowerCase();
    if (!lowered) return merged;
    return merged.filter((event) =>
      [event.type, event.project, event.service, event.message]
        .filter(Boolean)
        .some((field) => String(field).toLowerCase().includes(lowered)),
    );
  }, [merged, needle]);

  return (
    <Page title="Activity" lede="Every state change the daemon published, newest first.">
      <Panel padded={false}>
        <div className="panel-head">
          <h2>Last {Math.min(40, merged.length)} events</h2>
          <div className="spacer" />
          <Strip events={[...merged].reverse()} ticks={60} />
        </div>
        <div className="panel-head">
          <input
            className="input mono"
            style={{ width: 240 }}
            placeholder="filter by type, project or service"
            value={needle}
            onChange={(event) => setNeedle(event.target.value)}
          />
          <div className="spacer" />
          <Chip tone={connected ? "ok" : "idle"} label={connected ? "STREAMING" : "NOT STREAMING"} />
          <Button small variant="quiet" onClick={history.reload}>
            Reload history
          </Button>
        </div>

        {visible.length === 0 ? (
          <Empty>{merged.length === 0 ? "Nothing has happened yet." : "Nothing matches this filter."}</Empty>
        ) : (
          <table>
            <thead>
              <tr>
                <th className="num">Seq</th>
                <th>Time</th>
                <th>Type</th>
                <th>Project</th>
                <th>Service</th>
                <th>Message</th>
              </tr>
            </thead>
            <tbody>
              {visible.slice(0, 300).map((event) => (
                <tr key={event.seq}>
                  <td className="num faint">{event.seq}</td>
                  <td>{clockTime(event.timestamp)}</td>
                  <td>{event.type}</td>
                  <td>
                    {event.project ? (
                      <button type="button" className="link" onClick={() => navigate({ page: "project", id: event.project ?? "" })}>
                        {event.project}
                      </button>
                    ) : (
                      "—"
                    )}
                  </td>
                  <td>{event.service || "—"}</td>
                  <td>{event.message || "—"}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Panel>
    </Page>
  );
}
