"use client";

import { useMemo } from "react";
import { X } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { ErrorState, LoadingRows } from "@/components/shell/states";
import { BacktestStatusBadge } from "./status-badge";
import { MetricCard } from "./metric-card";
import { EquityPanel } from "./equity-panel";
import { TradesTable } from "./trades-table";
import { OrdersTable } from "./orders-table";
import { useBacktest } from "@/lib/api/hooks";
import {
  formatInt,
  formatMoney,
  formatNum,
  formatPct,
  formatRatioPct,
} from "@/lib/format";
import type { BacktestMetrics } from "@/lib/api/types";

function returnRatio(start: number, final: number): number | null {
  if (!start) return null;
  return (final - start) / start;
}

function MetricsGrid({
  m,
  startingBalance,
}: {
  m: BacktestMetrics;
  startingBalance: number;
}) {
  const ret = returnRatio(startingBalance, m.final_balance_usd);
  const fillRate =
    m.num_orders > 0 ? (m.num_filled_orders / m.num_orders) * 100 : null;
  return (
    <div
      className="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-4 xl:grid-cols-8"
      data-testid="metrics-grid"
    >
      <MetricCard
        label="Total return"
        value={formatRatioPct(ret)}
        rawValue={ret == null ? undefined : ret * 100}
        sub={formatMoney(m.total_pnl_usd)}
        tone={ret == null ? "neutral" : ret > 0 ? "positive" : ret < 0 ? "negative" : "neutral"}
        data-testid="metric-return"
      />
      <MetricCard
        label="Final balance"
        value={formatMoney(m.final_balance_usd)}
        rawValue={m.final_balance_usd}
        data-testid="metric-final-balance"
      />
      <MetricCard
        label="Total P&L"
        value={formatMoney(m.total_pnl_usd)}
        rawValue={m.total_pnl_usd}
        tone={m.total_pnl_usd > 0 ? "positive" : m.total_pnl_usd < 0 ? "negative" : "neutral"}
        data-testid="metric-total-pnl"
      />
      <MetricCard
        label="Sharpe"
        value={formatNum(m.sharpe)}
        rawValue={m.sharpe}
        data-testid="metric-sharpe"
      />
      <MetricCard
        label="Calmar"
        value={formatNum(m.calmar)}
        rawValue={m.calmar}
        data-testid="metric-calmar"
      />
      <MetricCard
        label="Max drawdown"
        value={formatPct(m.max_drawdown_pct)}
        rawValue={m.max_drawdown_pct}
        tone={m.max_drawdown_pct < 0 ? "negative" : "neutral"}
        data-testid="metric-max-drawdown"
      />
      <MetricCard
        label="Orders"
        value={formatInt(m.num_orders)}
        rawValue={m.num_orders}
        sub={
          fillRate == null
            ? `${formatInt(m.num_rejected_orders)} rejected`
            : `${fillRate.toFixed(0)}% filled · ${formatInt(m.num_rejected_orders)} rejected`
        }
        data-testid="metric-num-orders"
      />
      <MetricCard
        label="Positions"
        value={formatInt(m.num_positions)}
        rawValue={m.num_positions}
        sub={`${formatInt(m.num_filled_orders)} fills`}
        data-testid="metric-num-positions"
      />
    </div>
  );
}

/**
 * A backtest's results, rendered INLINE (formerly the `/backtests/[id]` route).
 * Per the "inline results" decision (docs/concept-alignment.md §3.4 ③) a run no
 * longer navigates to its own page — selecting a row in the runs table opens this
 * panel in place. `id` is the (positive integer) run id; the caller passes a
 * close handler to dismiss it.
 */
export function BacktestPanel({
  id,
  onClose,
}: {
  id: number;
  onClose?: () => void;
}) {
  const { data, isLoading, error, refetch } = useBacktest(id);

  const bt = data?.backtest;
  const strategyIds = useMemo(
    () => (data ? Object.keys(data.strategy_metrics) : []),
    [data],
  );

  return (
    <div
      className="space-y-4"
      data-testid="backtest-detail"
      data-backtest-id={id}
    >
      <div className="flex items-center justify-between gap-2">
        <div className="flex items-center gap-2">
          <span className="text-sm font-medium">
            {bt ? `Backtest #${bt.id}` : "Backtest"}
          </span>
          {bt ? <BacktestStatusBadge status={bt.status} /> : null}
          {bt ? (
            <span className="text-xs text-muted-foreground">
              {bt.start_date} → {bt.end_date} · {bt.kind}
            </span>
          ) : null}
        </div>
        {onClose ? (
          <Button
            variant="ghost"
            size="sm"
            onClick={onClose}
            data-testid="backtest-panel-close"
          >
            <X />
            Close
          </Button>
        ) : null}
      </div>

      {isLoading ? (
        <div className="space-y-4">
          <LoadingRows rows={2} data-testid="detail-loading" />
          <LoadingRows rows={6} />
        </div>
      ) : error ? (
        <ErrorState
          error={error}
          onRetry={() => refetch()}
          data-testid="detail-error"
        />
      ) : !data || !bt ? (
        <ErrorState
          error={new Error("Backtest not found.")}
          data-testid="detail-empty"
        />
      ) : (
        <>
          <MetricsGrid m={data.metrics} startingBalance={bt.starting_balance_usd} />

          <EquityPanel id={bt.id} strategies={strategyIds} />

          {strategyIds.length > 0 ? (
            <div
              className="flex flex-wrap items-center gap-2 text-xs"
              data-testid="strategy-chips"
            >
              <span className="text-muted-foreground">Strategies:</span>
              {strategyIds.map((s) => (
                <Badge key={s} variant="outline" data-testid={`strategy-chip-${s}`}>
                  {s}
                </Badge>
              ))}
            </div>
          ) : null}

          <TradesTable id={bt.id} />
          <OrdersTable id={bt.id} />
        </>
      )}
    </div>
  );
}
