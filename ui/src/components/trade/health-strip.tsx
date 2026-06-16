"use client";

import { useEffect, useState } from "react";
import { useLiveHealth } from "@/lib/api/hooks";
import { useLiveStream } from "@/lib/api/use-live-stream";
import { ApiError } from "@/lib/api/client";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { StatusDot } from "./live-badges";
import type { LiveHealth, WsPortfolioHealth } from "@/lib/api/types";
import { formatMoney, formatRatioPct, formatRelative } from "@/lib/format";

/**
 * Freshness color for the health snapshot given the minute cadence (spec:
 * PortfolioHealthActor publishes once a minute). Green when fresh, yellow at
 * >3min, red at >10min OR when daily_loss_halt is set.
 */
function freshness(h: LiveHealth | null, now: number): "green" | "yellow" | "red" | "gray" {
  if (!h) return "gray";
  if (h.daily_loss_halt) return "red";
  const ageMs = now - new Date(h.ts).getTime();
  if (Number.isNaN(ageMs)) return "gray";
  if (ageMs > 10 * 60 * 1000) return "red";
  if (ageMs > 3 * 60 * 1000) return "yellow";
  return "green";
}

function Metric({
  label,
  value,
  tone,
  testid,
}: {
  label: string;
  value: string;
  tone?: "pos" | "neg" | "neutral";
  testid: string;
}) {
  const cls =
    tone === "pos"
      ? "text-emerald-600 dark:text-emerald-400"
      : tone === "neg"
        ? "text-red-600 dark:text-red-400"
        : "text-foreground";
  return (
    <div className="flex min-w-[7rem] flex-col gap-0.5" data-testid={testid}>
      <span className="text-[10px] uppercase tracking-wide text-muted-foreground">
        {label}
      </span>
      <span className={`font-mono text-lg leading-none ${cls}`}>{value}</span>
    </div>
  );
}

/**
 * Portfolio-health strip: day P/L, halt headroom, concentration — the minute
 * cadence snapshot from PG, overlaid live by the `portfolio_health` WS frame.
 * In signal mode this is the flat-book informational NAV (day P&L 0), which is
 * the expected, correct reading — not an error.
 */
export function HealthStrip() {
  const q = useLiveHealth();
  const [pushed, setPushed] = useState<LiveHealth | null>(null);
  // A 1s ticking clock so the freshness dot and "as of" age stay live between
  // pushes without re-fetching.
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(id);
  }, []);

  useLiveStream({
    onPortfolioHealth: (p: WsPortfolioHealth) => {
      setPushed({
        day_pnl: p.day_pnl,
        day_pnl_pct: p.day_pnl_pct,
        daily_loss_halt: p.daily_loss_halt,
        halt_headroom_pct: p.halt_headroom_pct,
        concentration_pct: p.concentration_pct,
        // ts_event is epoch-ns; convert to an RFC3339 string for the formatters.
        ts: new Date(Math.floor(p.ts_event / 1e6)).toISOString(),
      });
    },
  });

  // Prefer the freshest of (poll snapshot, WS push) by timestamp.
  const polled = q.data ?? null;
  const health =
    pushed && polled
      ? new Date(pushed.ts).getTime() >= new Date(polled.ts).getTime()
        ? pushed
        : polled
      : (pushed ?? polled);

  if (q.isLoading && !health) {
    return (
      <Card data-testid="live-health" data-panel="health-strip">
        <CardHeader>
          <CardTitle className="text-sm">Portfolio health</CardTitle>
        </CardHeader>
        <CardContent>
          <Skeleton className="h-12 w-full" />
        </CardContent>
      </Card>
    );
  }

  const noHealth =
    q.error instanceof ApiError &&
    (q.error.status === 503 || q.error.code === "no_health");

  if (!health && noHealth) {
    return (
      <Card
        data-testid="live-health"
        data-panel="health-strip"
        data-state="no-health"
        data-daily-loss-halt="false"
      >
        <CardHeader>
          <CardTitle className="text-sm">Portfolio health</CardTitle>
        </CardHeader>
        <CardContent>
          <p className="py-2 text-xs text-muted-foreground">
            No health snapshot yet — no live producer running. In signal mode the
            book is flat (day P&L 0) once a session starts.
          </p>
        </CardContent>
      </Card>
    );
  }

  if (!health) {
    return (
      <Card data-testid="live-health" data-panel="health-strip" data-state="error">
        <CardHeader>
          <CardTitle className="text-sm">Portfolio health</CardTitle>
        </CardHeader>
        <CardContent>
          <p className="py-2 text-xs text-destructive">
            Failed to load health{q.error ? `: ${q.error.message}` : ""}.
          </p>
        </CardContent>
      </Card>
    );
  }

  const dot = freshness(health, now);
  const pnlTone =
    health.day_pnl > 0 ? "pos" : health.day_pnl < 0 ? "neg" : "neutral";
  // Lower headroom is worse; flag yellow text under 2 points of room.
  const headroomLow = health.halt_headroom_pct < 0.02;

  return (
    <Card
      data-testid="live-health"
      data-panel="health-strip"
      data-state={health.daily_loss_halt ? "halted" : "ok"}
      data-daily-loss-halt={health.daily_loss_halt ? "true" : "false"}
    >
      <CardHeader>
        <CardTitle className="text-sm">Portfolio health</CardTitle>
        <div className="flex items-center gap-1.5 text-xs text-muted-foreground">
          <StatusDot color={dot} pulse={dot === "green"} />
          <span data-testid="health-asof">
            as of {formatRelative(health.ts, now)}
          </span>
        </div>
      </CardHeader>
      <CardContent>
        <div className="flex flex-wrap items-end gap-x-8 gap-y-4">
          <Metric
            label="Day P/L"
            value={formatMoney(health.day_pnl)}
            tone={pnlTone}
            testid="live-health-day-pnl"
          />
          <Metric
            label="Day P/L %"
            value={formatRatioPct(health.day_pnl_pct)}
            tone={pnlTone}
            testid="health-day-pnl-pct"
          />
          <Metric
            label="Halt headroom"
            value={formatRatioPct(health.halt_headroom_pct)}
            tone={headroomLow ? "neg" : "neutral"}
            testid="health-headroom"
          />
          <Metric
            label="Concentration"
            value={formatRatioPct(health.concentration_pct)}
            testid="health-concentration"
          />
          <div className="flex min-w-[7rem] flex-col gap-0.5" data-testid="health-loss-halt">
            <span className="text-[10px] uppercase tracking-wide text-muted-foreground">
              Daily-loss halt
            </span>
            <span
              className={`font-mono text-lg leading-none ${
                health.daily_loss_halt
                  ? "text-red-600 dark:text-red-400"
                  : "text-emerald-600 dark:text-emerald-400"
              }`}
            >
              {health.daily_loss_halt ? "ACTIVE" : "clear"}
            </span>
          </div>
        </div>
      </CardContent>
    </Card>
  );
}
