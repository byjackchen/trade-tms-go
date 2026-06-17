"use client";

import { useEffect, useState } from "react";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { ErrorState, LoadingRows } from "@/components/shell/states";
import { Skeleton } from "@/components/ui/skeleton";
import { StatusDot } from "@/components/portfolio/live-badges";
import { useUiMode } from "@/components/shell/ui-mode-provider";
import { cn } from "@/lib/utils";
import {
  useSystem,
  useSystemHealth,
  useLiveSession,
  useLiveHealth,
} from "@/lib/api/hooks";
import { useJobStream } from "@/lib/api/use-job-stream";
import { hasSession } from "@/lib/api/types";
import { ApiError } from "@/lib/api/client";
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

function ConnRow({
  label,
  dot,
  detail,
  testid,
}: {
  label: string;
  dot: Dot;
  detail?: string;
  testid: string;
}) {
  return (
    <div
      className="flex items-center justify-between gap-3 rounded-lg border border-border px-3 py-2"
      data-testid={testid}
      data-dot={dot}
    >
      <div className="flex items-center gap-2">
        <StatusDot color={dot} pulse={dot === "green"} />
        <span className="text-sm font-medium">{label}</span>
      </div>
      <span className="truncate text-xs text-muted-foreground" title={detail}>
        {detail ?? "—"}
      </span>
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

/**
 * System health — the single, merged control-plane status view (replaces the
 * former duplicate `ops/system-health` grid and `trade/system-panel` connection
 * panel). It is driven by GET /api/v1/system (the bearer-guarded aggregate:
 * component grid + structured metrics + overall rollup) cross-checked against
 * the public /healthz proxy (Postgres / Redis reachability + API build version)
 * and the live WS bridge connection state.
 *
 * Layout:
 *   - Component grid       — postgres / redis / moomoo feed / job queue / data
 *     freshness / live sessions (+ scheduler when exposed), with the rollup dot.
 *   - Connections          — dependency reachability from /healthz, the inferred
 *     moomoo data-feed freshness, and the live event bridge (WS) state.
 *   - Metrics & transport  — queued/running jobs, active sessions, latest bar,
 *     last sync, live mode and API version.
 *
 * The aggregate /api/v1/system endpoint is always HTTP 200 (degradation is in
 * the body), so the grid renders red/yellow/green dots rather than throwing.
 */
export function SystemHealth() {
  const system = useSystem();
  const health = useSystemHealth();
  const sessionQ = useLiveSession();
  const liveHealthQ = useLiveHealth();
  const { state: bridgeState } = useJobStream({});
  const { mode } = useUiMode();
  const mobile = mode === "mobile";

  // A ticking clock so the inferred data-feed freshness re-evaluates without a
  // re-fetch (keeps Date.now() out of render — purity rule).
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), 5000);
    return () => clearInterval(id);
  }, []);

  const comps = system.data?.components;
  const metrics = system.data?.metrics;
  const rollup = system.data?.status;

  const bridgeDot: Dot =
    bridgeState === "open"
      ? "green"
      : bridgeState === "connecting"
        ? "yellow"
        : "red";

  const session = hasSession(sessionQ.data) ? sessionQ.data : null;
  const noReader =
    sessionQ.error instanceof ApiError && sessionQ.error.status === 503;

  // ---- Postgres ----
  const pg = health.data?.deps?.postgres;
  const pgDot: Dot = !health.data ? "gray" : pg?.ok ? "green" : "red";

  // ---- Redis ----
  const redis = health.data?.deps?.redis;
  const redisConfigured = !(redis && redis.error === "not configured");
  const redisDot: Dot = !health.data
    ? "gray"
    : !redisConfigured
      ? "yellow"
      : redis?.ok
        ? "green"
        : "red";

  // ---- moomoo data feed ----
  // Prefer the authoritative aggregate /api/v1/system component (the API infers
  // it server-side from the latest running session + health freshness). Fall
  // back to the client-side inference when the aggregate endpoint is unavailable
  // (older API / signal-only deployment without a system reader).
  const feedComp = comps?.moomoo_feed;
  let feedDot: Dot;
  let feedDetail: string;
  if (feedComp) {
    feedDot = statusToDot(feedComp.status);
    feedDetail = feedComp.detail ?? feedComp.status;
  } else {
    feedDot = "gray";
    feedDetail = "no session";
    if (session) {
      if (session.status === "RUNNING") {
        const lh = liveHealthQ.data;
        const ageMs = lh ? now - new Date(lh.ts).getTime() : Infinity;
        if (Number.isFinite(ageMs) && ageMs < 5 * 60 * 1000) {
          feedDot = "green";
          feedDetail = "data flowing";
        } else {
          feedDot = "yellow";
          feedDetail = "running — awaiting bars";
        }
      } else {
        feedDot = "gray";
        feedDetail = session.status.toLowerCase();
      }
    } else if (noReader) {
      feedDetail = "reader not configured";
    }
  }

  const version = system.data?.version ?? health.data?.version ?? "unknown";

  return (
    <div className="space-y-4" data-testid="systems-health">
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
              className={cn(
                "grid grid-cols-1 gap-3",
                !mobile && "sm:grid-cols-2 lg:grid-cols-3",
              )}
              data-testid="system-health-grid"
            >
              {COMPONENT_ORDER.filter((c) =>
                c.key === "scheduler" ? comps?.[c.key] : true,
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

      <Card data-testid="system-panel">
        <CardHeader>
          <CardTitle className="text-sm">Connections</CardTitle>
          <span
            className="text-xs text-muted-foreground"
            data-testid="system-version"
          >
            {health.isLoading
              ? "…"
              : health.data?.version
                ? `API ${health.data.version}`
                : "API version unknown"}
          </span>
        </CardHeader>
        <CardContent className="space-y-2">
          {health.isLoading ? (
            <Skeleton className="h-24 w-full" />
          ) : (
            <>
              <ConnRow
                label="moomoo data feed"
                dot={feedDot}
                detail={feedDetail}
                testid="conn-moomoo"
              />
              <ConnRow
                label="Redis (transport)"
                dot={redisDot}
                detail={
                  !redisConfigured
                    ? "not configured — WS degraded"
                    : redis?.ok
                      ? "connected"
                      : (redis?.error ?? "down")
                }
                testid="conn-redis"
              />
              <ConnRow
                label="Postgres (truth)"
                dot={pgDot}
                detail={pg?.ok ? "connected" : (pg?.error ?? "down")}
                testid="conn-postgres"
              />
              <ConnRow
                label="Live event bridge (WS)"
                dot={bridgeDot}
                detail={bridgeState}
                testid="conn-bridge"
              />
              {session ? (
                <div
                  className="mt-2 rounded-lg border border-border px-3 py-2 text-xs text-muted-foreground"
                  data-testid="system-session-detail"
                >
                  Session #{session.id} · trader{" "}
                  <span className="font-mono">{session.trader_id}</span> · started{" "}
                  <span title={formatTs(session.started_at)}>
                    {formatRelative(session.started_at)}
                  </span>
                </div>
              ) : null}
            </>
          )}
        </CardContent>
      </Card>

      <Card data-testid="system-metrics-card">
        <CardHeader>
          <CardTitle className="text-sm">Metrics &amp; transport</CardTitle>
        </CardHeader>
        <CardContent className="space-y-3">
          <div
            className={cn(
              "grid gap-3",
              mobile ? "grid-cols-1" : "grid-cols-2 lg:grid-cols-4",
            )}
          >
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
            <Metric label="API version" value={version} testid="metric-version" />
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
