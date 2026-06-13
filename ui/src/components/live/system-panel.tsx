"use client";

import { useEffect, useState } from "react";
import { useSystemHealth, useLiveSession, useLiveHealth } from "@/lib/api/hooks";
import { useLiveStream } from "@/lib/api/use-live-stream";
import { hasSession } from "@/lib/api/types";
import { ApiError } from "@/lib/api/client";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { StatusDot } from "./live-badges";
import { formatRelative, formatTs } from "@/lib/format";

type Dot = "green" | "yellow" | "red" | "gray";

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

  // ---- moomoo data feed (inferred) ----
  let feedDot: Dot = "gray";
  let feedDetail = "no session";
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
