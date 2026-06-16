"use client";

import { useManualTradeAccount } from "@/lib/api/hooks";
import { useLiveStream } from "@/lib/api/use-live-stream";
import { ApiError } from "@/lib/api/client";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { EmptyState } from "@/components/shell/states";
import { formatMoney } from "@/lib/format";

function Metric({
  label,
  value,
  tone,
  testid,
  dayPnlUsd,
  hint,
}: {
  label: string;
  value: string;
  tone?: "pos" | "neg" | "neutral";
  testid: string;
  dayPnlUsd?: number;
  hint?: string;
}) {
  const cls =
    tone === "pos"
      ? "text-emerald-600 dark:text-emerald-400"
      : tone === "neg"
        ? "text-red-600 dark:text-red-400"
        : "text-foreground";
  return (
    <div
      className="flex min-w-[8rem] flex-col gap-0.5"
      data-testid={testid}
      data-day-pnl-usd={dayPnlUsd}
    >
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
 * Manual-desk ACCOUNT panel: buying power (available funds) + day P&L for the
 * account the desk trades against (GET /api/v1/trade/account — the MANUAL desk's
 * own account, e.g. the $100k paper book). This is deliberately NOT the strategy
 * session's /live/account, which reads flat $0 in signal mode, nor its
 * `account_update` WS frame (same flat session book) — the desk account is the
 * manual book, kept fresh by the 15s poll. The day-P&L card exposes the raw USD
 * as `data-day-pnl-usd` for an exact, locale-independent comparison.
 */
export function ManualAccountPanel() {
  const acctQ = useManualTradeAccount();
  // The connection indicator still rides the live stream; the account VALUE is
  // the desk's own (polled), not the session's account_update frame.
  const { state } = useLiveStream({});

  const account = acctQ.data ?? null;

  const noReader =
    acctQ.error instanceof ApiError && acctQ.error.status === 503;

  if (acctQ.isLoading && !account) {
    return (
      <Card data-testid="manual-account" data-panel="manual-account">
        <CardHeader>
          <CardTitle className="text-sm">Account</CardTitle>
        </CardHeader>
        <CardContent>
          <Skeleton className="h-16 w-full" />
        </CardContent>
      </Card>
    );
  }

  if (!account) {
    return (
      <Card
        data-testid="manual-account"
        data-panel="manual-account"
        data-state={noReader ? "no-reader" : "error"}
      >
        <CardHeader>
          <CardTitle className="text-sm">Account</CardTitle>
        </CardHeader>
        <CardContent>
          {noReader ? (
            <EmptyState
              title="No manual trade desk connected"
              hint="Account / buying-power appears once a paper/live manual desk is attached."
              data-testid="manual-account-no-reader"
            />
          ) : (
            <p className="py-2 text-xs text-destructive">
              Failed to load account
              {acctQ.error ? `: ${acctQ.error.message}` : ""}.
            </p>
          )}
        </CardContent>
      </Card>
    );
  }

  const dayPnlTone =
    account.day_pnl > 0 ? "pos" : account.day_pnl < 0 ? "neg" : "neutral";

  return (
    <Card
      data-testid="manual-account"
      data-panel="manual-account"
      data-connected={state === "open" ? "true" : "false"}
    >
      <CardHeader>
        <CardTitle className="text-sm">Account</CardTitle>
      </CardHeader>
      <CardContent>
        <div className="flex flex-wrap items-end gap-x-8 gap-y-4">
          <Metric
            label="Buying power"
            value={formatMoney(account.available_funds)}
            testid="manual-account-buying-power"
            hint="available funds"
          />
          <Metric
            label="Total assets"
            value={formatMoney(account.total_assets)}
            testid="manual-account-total-assets"
          />
          <Metric
            label="Cash"
            value={formatMoney(account.cash)}
            testid="manual-account-cash"
          />
          <Metric
            label="Market value"
            value={formatMoney(account.market_value)}
            testid="manual-account-market-value"
          />
          <Metric
            label="Day P/L"
            value={formatMoney(account.day_pnl)}
            tone={dayPnlTone}
            testid="manual-account-day-pnl"
            dayPnlUsd={account.day_pnl}
          />
        </div>
      </CardContent>
    </Card>
  );
}
