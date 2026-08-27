// Logs viewer: one service's captured output, live.
//
// The stream is the default because a dev server's interesting output happens
// while you watch it. History is replayed first so the view is never empty.

import { useEffect, useMemo, useRef, useState } from "react";

import { useFeedSignal } from "../feed";
import { useLogStream, useResource } from "../hooks";
import { captureWarning, clockTime } from "../format";
import { useT } from "../i18n";
import { Button, Empty, Notice, Page, Panel } from "../ui";
import type { Service } from "../api/types";

export function LogsPage(props: { projectID?: string; service?: string }) {
  const t = useT();
  const signal = useFeedSignal();
  const projects = useResource((api) => api.projects(false), signal);
  const [projectID, setProjectID] = useState(props.projectID ?? "");
  const [serviceName, setServiceName] = useState(props.service ?? "");
  const [follow, setFollow] = useState(true);
  const [stream, setStream] = useState<"all" | "stdout" | "stderr">("all");
  const [needle, setNeedle] = useState("");
  const viewport = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    if (props.projectID) setProjectID(props.projectID);
    if (props.service) setServiceName(props.service);
  }, [props.projectID, props.service]);

  // Default to the first project so the page is useful without a selection.
  useEffect(() => {
    if (projectID || !projects.data || projects.data.length === 0) return;
    const first = projects.data[0];
    if (first) setProjectID(first.id);
  }, [projects.data, projectID]);

  const services = useResource(
    (api) => (projectID ? api.services(projectID) : Promise.resolve([] as Service[])),
    `${projectID}:${signal}`,
  );

  useEffect(() => {
    if (!services.data || services.data.length === 0) return;
    if (services.data.some((service) => service.name === serviceName)) return;
    const first = services.data[0];
    if (first) setServiceName(first.name);
  }, [services.data, serviceName]);

  const selected = services.data?.find((service) => service.name === serviceName);
  const { lines, connected } = useLogStream(projectID, serviceName, Boolean(projectID && serviceName));

  const visible = useMemo(() => {
    const lowered = needle.toLowerCase();
    return lines.filter((line) => {
      if (stream !== "all" && line.stream !== stream) return false;
      if (lowered && !line.message.toLowerCase().includes(lowered)) return false;
      return true;
    });
  }, [lines, stream, needle]);

  useEffect(() => {
    if (!follow) return;
    const element = viewport.current;
    if (element) element.scrollTop = element.scrollHeight;
  }, [visible, follow]);

  const capture = selected ? captureWarning(selected) : null;

  return (
    <Page title={t("logs.title")} lede={t("logs.lede")}>
      {capture ? <Notice tone="warn" title={t("logs.captureTitle")}>{t(capture)}</Notice> : null}
      {selected && selected.observability.log_capture === "none" ? (
        <Notice tone="info" title={t("logs.noneTitle")}>{t("logs.noneBody")}</Notice>
      ) : null}

      <Panel padded={false}>
        <div className="panel-head">
          <label className="row" style={{ gap: 6 }}>
            <span className="faint mono">{t("logs.project")}</span>
            <select
              className="input mono"
              style={{ width: 220 }}
              value={projectID}
              onChange={(event) => setProjectID(event.target.value)}
            >
              {(projects.data ?? []).map((project) => (
                <option key={project.id} value={project.id}>
                  {project.name}
                </option>
              ))}
            </select>
          </label>
          <label className="row" style={{ gap: 6 }}>
            <span className="faint mono">{t("logs.service")}</span>
            <select
              className="input mono"
              style={{ width: 180 }}
              value={serviceName}
              onChange={(event) => setServiceName(event.target.value)}
            >
              {(services.data ?? []).map((service) => (
                <option key={service.name} value={service.name}>
                  {service.name}
                </option>
              ))}
            </select>
          </label>
          <label className="row" style={{ gap: 6 }}>
            <span className="faint mono">{t("logs.stream")}</span>
            <select
              className="input mono"
              style={{ width: 110 }}
              value={stream}
              onChange={(event) => setStream(event.target.value as "all" | "stdout" | "stderr")}
            >
              <option value="all">{t("logs.all")}</option>
              <option value="stdout">stdout</option>
              <option value="stderr">stderr</option>
            </select>
          </label>
          <input
            className="input mono"
            style={{ width: 200 }}
            placeholder={t("logs.filter")}
            value={needle}
            onChange={(event) => setNeedle(event.target.value)}
          />
          <div className="spacer" />
          <span className={connected ? "mono t-ok" : "mono faint"}>
            {connected ? t("logs.live") : t("logs.notStreaming")}
          </span>
          <Button small variant={follow ? "primary" : undefined} onClick={() => setFollow(!follow)}>
            {follow ? t("logs.following") : t("logs.follow")}
          </Button>
        </div>

        <div className="log" ref={viewport} onWheel={() => setFollow(false)}>
          {visible.length === 0 ? (
            <Empty>{lines.length === 0 ? t("logs.emptyNone") : t("logs.emptyFilter")}</Empty>
          ) : (
            visible.map((line) => (
              <div className={`log-line ${line.stream}`} key={`${line.seq}`}>
                <time>{clockTime(line.timestamp)}</time>
                <span>{line.message}</span>
              </div>
            ))
          )}
        </div>
      </Panel>

      <p className="muted mono" style={{ marginTop: 10 }}>
        {t("logs.count", { shown: visible.length, total: lines.length })}
      </p>
    </Page>
  );
}
