"use client";

import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { ErrorState, LoadingRows } from "@/components/shell/states";
import { StatusDot } from "@/components/live/live-badges";
import { useSystem, useSystemHealth } from "@/lib/api/hooks";
import { useJobStream } from "@/lib/api/use-job-stream";
import { formatRelative, formatTs } from "@/lib/format";
import type { SystemComponent } from "@/lib/api/types";

type Dot = "green" | "yellow" | "red" | "gray";

/** Map a backend component status string to a status-dot color. */
function statusToDot(status: string | undefined): Dot {
  switch (status) {
    case "ok":
      return "green";
    case "degraded":
    case "unknown":
      return "yellow";
    case "down":
      return "red";
    default:
      // idle / not_configured / missing
      return "gray";
  }
}

/** The component rows the health grid renders, in a stable display order. The
 * `scheduler` row is rendered only when the API exposes it (the scheduler is a
 * separate node; this surfaces its last-run/next-run when present). */
const COMPONENT_ORDER: { key: string; label: string }[] = [
  { key: "postgres", label: "Postgres" },
  { key: "redis", label: "Redis" },
  { key: "moomoo_feed", label: "moomoo feed" },
  { key: "jobs", label: "Job queue" },
  { key: "data", label: "Data freshness" },
  { key: "sessions", label: "Live sessions" },
  { key: "scheduler", label: "Scheduler" },
];

function HealthCard({
  label,
  comp,
  testid,
}: {
  label: string;
  comp: SystemComponent | undefined;
  testid: string;
}) {
  const dot = statusToDot(comp?.status);
  return (
    <div
      className="rounded-lg border border-border px-3 py-3"
      data-testid={testid}
      data-status={comp?.status ?? "missing"}
      data-dot={dot}
    >
      <div className="flex items-center justify-between gap-2">
        <span className="text-sm font-medium">{label}</span>
        <StatusDot color={dot} pulse={dot === "green"} />
      </div>
      <div
        className="mt-1 truncate text-xs text-muted-foreground"
        title={comp?.detail}
        data-testid={`${testid}-detail`}
      >
        {comp?.detail ?? comp?.status ?? "—"}
      </div>
    </div>
  );
}

/**
 * System health: the component grid from GET /api/v1/system (postgres / redis /
 * moomoo feed / job-queue depth / data freshness / active sessions, plus the
 * scheduler when the API exposes it) and the structured metrics, with the
 * overall rollup status and the live WS-bridge connection state.
 *
 * The aggregate /api/v1/system endpoint is always HTTP 200 (degradation is in
 * the body), so the grid renders red/yellow/green dots rather than throwing.
 * Postgres + Redis reachability is cross-checked against the public /healthz
 * proxy for the bridge/version line.
 */
export function SystemHealth() {
  const system = useSystem();
  const health = useSystemHealth();
  const { state: bridgeState } = useJobStream({});

  const comps = system.data?.components;
  const metrics = system.data?.metrics;
  const rollup = system.data?.status;

  const bridgeDot: Dot =
    bridgeState === "open"
      ? "green"
      : bridgeState === "connecting"
        ? "yellow"
        : "red";

  return (
    <div className="space-y-4" data-testid="ops-system-health">
      <Card data-testid="system-health-card">
        <CardHeader>
          <div className="space-y-1">
            <CardTitle>System health</CardTitle>
            <CardDescription>
              Live component status across the control plane.
            </CardDescription>
          </div>
          {rollup ? (
            <div
              className="flex items-center gap-2"
              data-testid="system-rollup"
              data-status={rollup}
            >
              <StatusDot color={statusToDot(rollup)} pulse={rollup === "ok"} />
              <span className="text-sm font-medium capitalize">{rollup}</span>
            </div>
          ) : null}
        </CardHeader>
        <CardContent>
          {system.isLoading ? (
            <LoadingRows rows={4} data-testid="system-health-loading" />
          ) : system.error ? (
            <ErrorState
              error={system.error}
              onRetry={() => system.refetch()}
              data-testid="system-health-error"
            />
          ) : (
            <div
              className="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-3"
              data-testid="system-health-grid"
            >
              {COMPONENT_ORDER.filter(
                (c) => c.key === "scheduler" ? comps?.[c.key] : true,
              ).map((c) => (
                <HealthCard
                  key={c.key}
                  label={c.label}
                  comp={comps?.[c.key]}
                  testid={`health-${c.key}`}
                />
              ))}
            </div>
          )}
        </CardContent>
      </Card>

      <Card data-testid="system-metrics-card">
        <CardHeader>
          <CardTitle className="text-sm">Metrics &amp; transport</CardTitle>
        </CardHeader>
        <CardContent className="space-y-3">
          <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
            <Metric
              label="Queued jobs"
              value={metrics ? String(metrics.jobs_queued) : "—"}
              testid="metric-jobs-queued"
            />
            <Metric
              label="Running jobs"
              value={metrics ? String(metrics.jobs_running) : "—"}
              testid="metric-jobs-running"
            />
            <Metric
              label="Active sessions"
              value={metrics ? String(metrics.active_sessions) : "—"}
              testid="metric-active-sessions"
            />
            <Metric
              label="Latest bar"
              value={metrics?.latest_bar_date ?? "no bars"}
              testid="metric-latest-bar"
            />
            <Metric
              label="Last sync"
              value={
                metrics?.last_sync_at
                  ? formatRelative(metrics.last_sync_at)
                  : "never"
              }
              title={
                metrics?.last_sync_at ? formatTs(metrics.last_sync_at) : undefined
              }
              testid="metric-last-sync"
            />
            <Metric
              label="Live mode"
              value={metrics?.live_mode ?? "—"}
              testid="metric-live-mode"
            />
            <Metric
              label="API version"
              value={
                system.data?.version ?? health.data?.version ?? "unknown"
              }
              testid="metric-version"
            />
            <div
              className="rounded-lg border border-border px-3 py-2"
              data-testid="metric-ws-bridge"
              data-dot={bridgeDot}
            >
              <div className="text-xs text-muted-foreground">Event bridge</div>
              <div className="mt-0.5 flex items-center gap-1.5">
                <StatusDot color={bridgeDot} pulse={bridgeDot === "green"} />
                <span className="text-sm font-medium">{bridgeState}</span>
              </div>
            </div>
          </div>
        </CardContent>
      </Card>
    </div>
  );
}

function Metric({
  label,
  value,
  title,
  testid,
}: {
  label: string;
  value: string;
  title?: string;
  testid: string;
}) {
  return (
    <div className="rounded-lg border border-border px-3 py-2" data-testid={testid}>
      <div className="text-xs text-muted-foreground">{label}</div>
      <div className="mt-0.5 truncate text-sm font-medium" title={title ?? value}>
        {value}
      </div>
    </div>
  );
}
