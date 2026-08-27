// Shared components.
//
// The vocabulary is small on purpose: a chip states a status, a notice states a
// condition and what to do about it, and a strip shows recent change over time.
// Pages compose these rather than inventing their own treatments.

import type { ReactNode } from "react";

import type { DaemonEvent, EventType } from "./api/types";
import { clockTime, toneClass, type Tone } from "./format";

export function Page(props: {
  title: string;
  lede?: string;
  actions?: ReactNode;
  children: ReactNode;
}) {
  return (
    <>
      <header className="page-head">
        <div>
          <h1>{props.title}</h1>
          {props.lede ? <p>{props.lede}</p> : null}
        </div>
        {props.actions ? <div className="actions">{props.actions}</div> : null}
      </header>
      {props.children}
    </>
  );
}

export function Panel(props: { title?: string; aside?: ReactNode; padded?: boolean; children: ReactNode }) {
  return (
    <section className="panel">
      {props.title ? (
        <div className="panel-head">
          <h2>{props.title}</h2>
          <div className="spacer" />
          {props.aside}
        </div>
      ) : null}
      {props.padded === false ? props.children : <div className="panel-body">{props.children}</div>}
    </section>
  );
}

export function Chip(props: { tone: Tone; label: string; pulsing?: boolean }) {
  return (
    <span className={`chip ${toneClass(props.tone)}`}>
      <span className={props.pulsing ? "dot pulsing" : "dot"} />
      {props.label}
    </span>
  );
}

export function Fact(props: { label: string; children: ReactNode }) {
  return (
    <div className="fact">
      <dt>{props.label}</dt>
      <dd>{props.children}</dd>
    </div>
  );
}

export function Facts(props: { children: ReactNode }) {
  return <dl className="grid-facts">{props.children}</dl>;
}

export function Notice(props: {
  tone: Tone;
  title: string;
  children?: ReactNode;
  actions?: ReactNode;
}) {
  const variant =
    props.tone === "ok" ? "notice-ok" : props.tone === "bad" ? "notice-bad" : props.tone === "warn" ? "notice-warn" : "notice-info";
  return (
    <div className={`notice ${variant}`} style={{ marginBottom: 14 }}>
      <div className="notice-body">
        <strong>{props.title}</strong>
        {props.children ? <p>{props.children}</p> : null}
      </div>
      {props.actions ? <div className="actions">{props.actions}</div> : null}
    </div>
  );
}

export function Empty(props: { children: ReactNode }) {
  return <div className="empty">{props.children}</div>;
}

export function Button(props: {
  onClick: () => void;
  children: ReactNode;
  variant?: "primary" | "danger" | "quiet";
  small?: boolean;
  disabled?: boolean;
  title?: string;
}) {
  const classes = ["btn"];
  if (props.variant === "primary") classes.push("btn-primary");
  if (props.variant === "danger") classes.push("btn-danger");
  if (props.variant === "quiet") classes.push("btn-quiet");
  if (props.small) classes.push("btn-small");
  return (
    <button
      type="button"
      className={classes.join(" ")}
      onClick={props.onClick}
      disabled={props.disabled}
      title={props.title}
    >
      {props.children}
    </button>
  );
}

/**
 * Strip is the signature element: a strip-chart of the events DevMan has seen for
 * one service, newest on the right.
 *
 * Every tick is a real event, coloured by what it was and as tall as it is
 * significant, so a service that crashed and restarted twice looks different from
 * one that has been quietly running — without reading a single line of text.
 */
export function Strip(props: { events: DaemonEvent[]; ticks?: number }) {
  const width = props.ticks ?? 28;
  const recent = props.events.slice(-width);
  if (recent.length === 0) {
    return <span className="strip-empty">no events yet</span>;
  }
  return (
    <div className="strip" role="img" aria-label={`${recent.length} recent events`}>
      {recent.map((event) => {
        const shape = eventShape(event.type);
        return (
          <span
            key={event.seq}
            className={`strip-tick ${toneClass(shape.tone)}`}
            style={{ height: `${shape.height}%` }}
            title={`${clockTime(event.timestamp)} ${event.type}${event.message ? ` — ${event.message}` : ""}`}
          />
        );
      })}
    </div>
  );
}

/** eventShape maps an event to a colour and a height. Height encodes weight:
 *  a crash is full height, a health probe result is a low tick. */
function eventShape(type: EventType): { tone: Tone; height: number } {
  switch (type) {
    case "SERVICE_STARTED":
    case "PROJECT_STARTED":
      return { tone: "ok", height: 100 };
    case "SERVICE_STARTING":
    case "SERVICE_RESTART_SCHEDULED":
      return { tone: "warn", height: 55 };
    case "SERVICE_CRASHED":
    case "SERVICE_EXITED":
      return { tone: "bad", height: 100 };
    case "SERVICE_BLOCKED":
      return { tone: "blocked", height: 80 };
    case "SERVICE_STOPPED":
    case "SERVICE_STOPPING":
    case "PROJECT_STOPPED":
      return { tone: "idle", height: 45 };
    case "SERVICE_ADOPTED":
      return { tone: "info", height: 70 };
    case "HEALTH_CHANGED":
      return { tone: "info", height: 30 };
    case "PORT_BOUND":
      return { tone: "ok", height: 30 };
    case "PORT_CONFLICT":
      return { tone: "bad", height: 70 };
    default:
      return { tone: "idle", height: 25 };
  }
}
