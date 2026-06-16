"use client";

import { useState } from "react";
import { useLiveAccount, useLiveHealth } from "@/lib/api/hooks";
import { useLiveStream } from "@/lib/api/use-live-stream";
import { ApiError } from "@/lib/api/client";
import {
  Card,
  CardContent,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { EmptyState } from "@/components/shell/states";
import { StatusDot } from "./live-badges";
import type { LiveAccount, WsAccountUpdate } from "@/lib/api/types";
import { formatMoney, formatRatioPct } from "@/lib/format";

function Metric({
  label,
  value,
  tone,
  testid,
  hint,
}: {
  label: string;
  value: string;
  tone?: "pos" | "neg" | "neutral";
  testid: string;
  hint?: string;
}) {
  const cls =
    tone === "pos"
      ? "text-emerald-600 dark:text-emerald-400"
      : tone === "neg"
        ? "text-red-600 dark:text-red-400"
        : "text-foreground";
  return (
    <div className="flex min-w-[8rem] flex-col gap-0.5" data-testid={testid}>
      <span className="text-[10px] uppercase tracking-wide text-muted-foreground">
        {label}
      </span>
      <span className={`font-mono text-lg leading-none ${cls}`}>{value}</span>
      {hint ? (
        <span className="text-[10px] text-muted-foreground">{hint}</span>
      ) : null}
    </div>
  );
}

/**
 * Account panel: funds / buying power, market value, day P&L, and the
 * daily-loss-halt headroom (the distance to the −10% NAV halt threshold,
 * portfolio-risk.md). The account snapshot hydrates from PG
 * (GET /api/v1/live/account) and is overlaid live by the `account_update` WS
 * frame (broker funds / buying power). The halt headroom comes from the
 * portfolio-health snapshot (the canonical −10% computation), so the two panels
 * never disagree.
 */
export function AccountPanel({ accountId }: { accountId?: string } = {}) {
  const acctQ = useLiveAccount(accountId);
  const healthQ = useLiveHealth();
  const [pushed, setPushed] = useState<LiveAccount | null>(null);

  const { state } = useLiveStream({
    onAccountUpdate: (p: WsAccountUpdate) => {
      setPushed({
        total_assets: p.total_assets,
        cash: p.cash,
        available_funds: p.available_funds,
        market_value: p.market_value,
        day_pnl: p.day_pnl,
        ts: new Date(Math.floor(p.ts_event / 1e6)).toISOString(),
      });
    },
  });

  const polled = acctQ.data ?? null;
  // Prefer the freshest of (poll snapshot, WS push) by timestamp.
  const account =
    pushed && polled
      ? new Date(pushed.ts).getTime() >= new Date(polled.ts).getTime()
        ? pushed
        : polled
      : (pushed ?? polled);

  const health = healthQ.data ?? null;

  const noReader =
    acctQ.error instanceof ApiError && acctQ.error.status === 503;

  if (acctQ.isLoading && !account) {
    return (
      <Card data-testid="live-account" data-panel="account-panel">
        <CardHeader>
          <CardTitle className="text-sm">Account</CardTitle>
        </CardHeader>
        <CardContent>
          <Skeleton className="h-16 w-full" />
        </CardContent>
      </Card>
    );
  }

  if (!account && noReader) {
    return (
      <Card
        data-testid="live-account"
        data-panel="account-panel"
        data-state="no-reader"
      >
        <CardHeader>
          <CardTitle className="text-sm">Account</CardTitle>
        </CardHeader>
        <CardContent>
          <EmptyState
            title="Live trading reader not configured"
            hint="Account / buying-power appears once a paper/live session runs."
            data-testid="account-no-reader"
          />
        </CardContent>
      </Card>
    );
  }

  if (!account) {
    return (
      <Card
        data-testid="live-account"
        data-panel="account-panel"
        data-state="error"
      >
        <CardHeader>
          <CardTitle className="text-sm">Account</CardTitle>
        </CardHeader>
        <CardContent>
          <p className="py-2 text-xs text-destructive">
            Failed to load account
            {acctQ.error ? `: ${acctQ.error.message}` : ""}.
          </p>
        </CardContent>
      </Card>
    );
  }

  const dayPnlTone =
    account.day_pnl > 0 ? "pos" : account.day_pnl < 0 ? "neg" : "neutral";
  const halted = health?.daily_loss_halt ?? false;
  // Lower headroom is worse; flag red under 2 points of room to the −10% halt.
  const headroom = health?.halt_headroom_pct ?? null;
  const headroomLow = headroom != null && headroom < 0.02;
  const dot = halted ? "red" : headroomLow ? "yellow" : "green";

  return (
    <Card
      data-testid="live-account"
      data-panel="account-panel"
      data-state={halted ? "halted" : "ok"}
      data-daily-loss-halt={halted ? "true" : "false"}
      data-connected={state === "open" ? "true" : "false"}
    >
      <CardHeader>
        <CardTitle className="text-sm">Account</CardTitle>
        <div className="flex items-center gap-1.5 text-xs text-muted-foreground">
          <StatusDot color={dot} />
          <span>
            {halted ? "daily-loss halt ACTIVE" : "within risk budget"}
          </span>
        </div>
      </CardHeader>
      <CardContent>
        <div className="flex flex-wrap items-end gap-x-8 gap-y-4">
          <Metric
            label="Total assets"
            value={formatMoney(account.total_assets)}
            testid="account-total-assets"
          />
          <Metric
            label="Buying power"
            value={formatMoney(account.available_funds)}
            testid="account-buying-power"
            hint="available funds"
          />
          <Metric
            label="Cash"
            value={formatMoney(account.cash)}
            testid="account-cash"
          />
          <Metric
            label="Market value"
            value={formatMoney(account.market_value)}
            testid="account-market-value"
          />
          {/* `live-account-day-pnl` + `data-day-pnl-usd` is the e2e contract
              (spec 24): the day-P&L card exposes the numeric for an exact,
              locale-independent comparison against Σ realized in the DB. */}
          <div
            className="flex min-w-[8rem] flex-col gap-0.5"
            data-testid="live-account-day-pnl"
            data-day-pnl-usd={account.day_pnl}
          >
            <span className="text-[10px] uppercase tracking-wide text-muted-foreground">
              Day P/L
            </span>
            <span
              className={`font-mono text-lg leading-none ${
                dayPnlTone === "pos"
                  ? "text-emerald-600 dark:text-emerald-400"
                  : dayPnlTone === "neg"
                    ? "text-red-600 dark:text-red-400"
                    : "text-foreground"
              }`}
            >
              {formatMoney(account.day_pnl)}
            </span>
          </div>
          <div
            className="flex min-w-[8rem] flex-col gap-0.5"
            data-testid="account-halt-headroom"
          >
            <span className="text-[10px] uppercase tracking-wide text-muted-foreground">
              Loss-halt headroom
            </span>
            <span
              className={`font-mono text-lg leading-none ${
                halted
                  ? "text-red-600 dark:text-red-400"
                  : headroomLow
                    ? "text-amber-600 dark:text-amber-400"
                    : "text-foreground"
              }`}
            >
              {headroom != null ? formatRatioPct(headroom) : "—"}
            </span>
            <span className="text-[10px] text-muted-foreground">
              to −10% NAV halt
            </span>
          </div>
        </div>
      </CardContent>
    </Card>
  );
}
