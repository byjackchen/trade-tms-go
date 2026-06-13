"use client";

import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
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
          <Table data-testid="trades-table">
            <TableHeader>
              <TableRow>
                <TableHead>Strategy</TableHead>
                <TableHead>Symbol</TableHead>
                <TableHead>Side</TableHead>
                <TableHead className="text-right">Qty</TableHead>
                <TableHead>Entry</TableHead>
                <TableHead className="text-right">Entry px</TableHead>
                <TableHead>Exit</TableHead>
                <TableHead className="text-right">Exit px</TableHead>
                <TableHead className="text-right">Realized P&amp;L</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {trades.map((t) => (
                <TableRow key={t.id} data-testid={`trade-row-${t.id}`}>
                  <TableCell
                    className="font-mono text-xs"
                    data-testid="trade-strategy"
                  >
                    {t.strategy_id}
                  </TableCell>
                  <TableCell className="font-mono text-xs" data-testid="trade-symbol">
                    {t.symbol}
                  </TableCell>
                  <TableCell>
                    <SideBadge side={t.side} />
                  </TableCell>
                  <TableCell className="text-right tabular-nums">
                    {formatInt(t.qty)}
                  </TableCell>
                  <TableCell
                    className="text-xs text-muted-foreground"
                    title={formatTs(t.entry_ts)}
                  >
                    {t.entry_ts.slice(0, 10)}
                  </TableCell>
                  <TableCell className="text-right tabular-nums">
                    {formatNum(t.entry_px, 4)}
                  </TableCell>
                  <TableCell
                    className="text-xs text-muted-foreground"
                    title={t.exit_ts ? formatTs(t.exit_ts) : "open"}
                  >
                    {t.exit_ts ? (
                      t.exit_ts.slice(0, 10)
                    ) : (
                      <Badge variant="muted" data-testid="trade-open">
                        open
                      </Badge>
                    )}
                  </TableCell>
                  <TableCell className="text-right tabular-nums">
                    {t.exit_px == null ? "—" : formatNum(t.exit_px, 4)}
                  </TableCell>
                  <TableCell
                    className={`text-right tabular-nums ${pnlClass(t.realized_pnl_usd) ?? ""}`}
                    data-testid="trade-pnl"
                  >
                    {formatMoney(t.realized_pnl_usd)}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </CardContent>
    </Card>
  );
}
