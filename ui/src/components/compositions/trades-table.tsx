"use client";

import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  ResponsiveTable,
  type ColumnDef,
} from "@/components/ui/responsive-table";
import { Badge } from "@/components/ui/badge";
import { ErrorState, LoadingRows, EmptyState } from "@/components/shell/states";
import { useBacktestTrades } from "@/lib/api/hooks";
import { formatInt, formatMoney, formatNum, formatTs } from "@/lib/format";
import type { BacktestTrade } from "@/lib/api/types";

function SideBadge({ side }: { side: string }) {
  const s = side.toUpperCase();
  return (
    <Badge variant={s === "SHORT" ? "warning" : "secondary"}>{s}</Badge>
  );
}

function pnlClass(v: number): string | undefined {
  return v > 0
    ? "text-emerald-600 dark:text-emerald-400"
    : v < 0
      ? "text-destructive"
      : undefined;
}

/** Column definitions for the trades ResponsiveTable. Symbol + Realized P&L are
 * the mobile-card primaries; the rest (full parity) fold under "More". */
const COLUMNS: ColumnDef<BacktestTrade>[] = [
  {
    key: "strategy",
    header: "Strategy",
    className: "font-mono text-xs",
    render: (t) => <span data-testid="trade-strategy">{t.strategy_id}</span>,
  },
  {
    key: "symbol",
    header: "Symbol",
    primary: true,
    className: "font-mono text-xs",
    render: (t) => <span data-testid="trade-symbol">{t.symbol}</span>,
  },
  {
    key: "side",
    header: "Side",
    render: (t) => <SideBadge side={t.side} />,
  },
  {
    key: "qty",
    header: "Qty",
    align: "right",
    className: "tabular-nums",
    render: (t) => formatInt(t.qty),
  },
  {
    key: "entry",
    header: "Entry",
    className: "text-xs text-muted-foreground",
    render: (t) => (
      <span title={formatTs(t.entry_ts)}>{t.entry_ts.slice(0, 10)}</span>
    ),
  },
  {
    key: "entry_px",
    header: "Entry px",
    align: "right",
    className: "tabular-nums",
    render: (t) => formatNum(t.entry_px, 4),
  },
  {
    key: "exit",
    header: "Exit",
    className: "text-xs text-muted-foreground",
    render: (t) => (
      <span title={t.exit_ts ? formatTs(t.exit_ts) : "open"}>
        {t.exit_ts ? (
          t.exit_ts.slice(0, 10)
        ) : (
          <Badge variant="muted" data-testid="trade-open">
            open
          </Badge>
        )}
      </span>
    ),
  },
  {
    key: "exit_px",
    header: "Exit px",
    align: "right",
    className: "tabular-nums",
    render: (t) => (t.exit_px == null ? "—" : formatNum(t.exit_px, 4)),
  },
  {
    key: "pnl",
    header: "Realized P&L",
    primary: true,
    align: "right",
    render: (t) => (
      <span
        className={`tabular-nums ${pnlClass(t.realized_pnl_usd) ?? ""}`}
        data-testid="trade-pnl"
      >
        {formatMoney(t.realized_pnl_usd)}
      </span>
    ),
  },
];

export function TradesTable({ id }: { id: number }) {
  const { data, isLoading, error, refetch } = useBacktestTrades(id);
  const trades: BacktestTrade[] = data?.trades ?? [];

  return (
    <Card data-testid="trades-card">
      <CardHeader className="flex-col items-start gap-1">
        <CardTitle>Trades</CardTitle>
        <CardDescription>
          Round-trip trades by strategy and symbol. Open positions show no exit.
        </CardDescription>
      </CardHeader>
      <CardContent>
        {isLoading ? (
          <LoadingRows rows={4} data-testid="trades-loading" />
        ) : error ? (
          <ErrorState
            error={error}
            onRetry={() => refetch()}
            data-testid="trades-error"
          />
        ) : trades.length === 0 ? (
          <EmptyState
            title="No trades"
            hint="This run produced no round-trip trades."
            data-testid="trades-empty"
          />
        ) : (
          <ResponsiveTable
            columns={COLUMNS}
            rows={trades}
            rowKey={(t) => t.id}
            rowTestId={(t) => `trade-row-${t.id}`}
            data-testid="trades-table"
          />
        )}
      </CardContent>
    </Card>
  );
}
