"use client";

import { useEffect, useState } from "react";
import {
  useSystemHealth,
  useSystem,
  useLiveSession,
  useLiveHealth,
} from "@/lib/api/hooks";
import { useLiveStream } from "@/lib/api/use-live-stream";
import { hasSession } from "@/lib/api/types";
import { ApiError } from "@/lib/api/client";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { StatusDot } from "./live-badges";
import { formatRelative, formatTs } from "@/lib/format";

type Dot = "green" | "yellow" | "red" | "gray";

/** Map a backend component status string to a status dot color. */
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

function MetricTile({
  label,
  value,
  testid,
}: {
  label: string;
  value: string;
  testid: string;
}) {
  return (
    <div
      className="rounded-lg border border-border px-3 py-2"
      data-testid={testid}
    >
      <div className="text-xs text-muted-foreground">{label}</div>
      <div className="truncate text-sm font-medium" title={value}>
        {value}
      </div>
    </div>
  );
}

/**
 * System page body: dependency connection status (moomoo data feed / Redis /
 * Postgres), the live WS bridge, and the API build version.
 *
 * Postgres + Redis come from the upstream public /healthz (proxied server-side).
 * The moomoo data-feed status is inferred from the session + health freshness
 * (the API does not expose the OpenD socket directly in P5); a RUNNING session
 * with a fresh health snapshot means data is flowing.
 */
export function SystemPanel() {
  const health = useSystemHealth();
  const system = useSystem();
  const sessionQ = useLiveSession();
  const liveHealthQ = useLiveHealth();
  const { state } = useLiveStream({});
  // A ticking clock so the inferred data-feed freshness re-evaluates without a
  // re-fetch (keeps Date.now() out of render — purity rule).
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), 5000);
    return () => clearInterval(id);
  }, []);

  const session = hasSession(sessionQ.data) ? sessionQ.data : null;
  const noReader =
    sessionQ.error instanceof ApiError && sessionQ.error.status === 503;

  // ---- Postgres ----
  const pg = health.data?.deps?.postgres;
  const pgDot: Dot = !health.data
    ? "gray"
    : pg?.ok
      ? "green"
      : "red";

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
  const feedComp = system.data?.components?.moomoo_feed;
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

  const metrics = system.data?.metrics;

  // ---- WS bridge ----
  const bridgeDot: Dot =
    state === "open" ? "green" : state === "connecting" ? "yellow" : "red";

  return (
    <Card data-testid="system-panel">
      <CardHeader>
        <CardTitle className="text-sm">Connections</CardTitle>
        <span className="text-xs text-muted-foreground" data-testid="system-version">
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
              detail={state}
              testid="conn-bridge"
            />
            {metrics ? (
              <div
                className="mt-2 grid grid-cols-2 gap-2"
                data-testid="system-metrics"
              >
                <MetricTile
                  label="Job queue"
                  value={`${metrics.jobs_queued} queued · ${metrics.jobs_running} running`}
                  testid="metric-jobs"
                />
                <MetricTile
                  label="Active sessions"
                  value={String(metrics.active_sessions)}
                  testid="metric-sessions"
                />
                <MetricTile
                  label="Data freshness"
                  value={metrics.latest_bar_date ?? "no bars"}
                  testid="metric-data"
                />
                <MetricTile
                  label="Live mode"
                  value={metrics.live_mode ?? "—"}
                  testid="metric-mode"
                />
              </div>
            ) : null}
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
  );
}
