"use client";

import { useMemo, useState } from "react";
import { useLivePositions } from "@/lib/api/hooks";
import { useLiveStream } from "@/lib/api/use-live-stream";
import { ApiError } from "@/lib/api/client";
import {
  Card,
  CardContent,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  Table,
  TableBody,
  TableCell,
  TableFooter,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Skeleton } from "@/components/ui/skeleton";
import { EmptyState, ErrorState } from "@/components/shell/states";
import { DisconnectedBanner } from "./disconnected-banner";
import { SideBadge } from "./live-badges";
import type {
  LiveTradePosition,
  WsLivePosition,
  WsLivePositionRow,
} from "@/lib/api/types";
import { formatInt, formatMoney } from "@/lib/format";

/**
 * A normalized position row keyed by (strategy_id, symbol). Market value uses
 * the average entry price as the mark when no live quote is on the wire (the
 * account stream carries the broker mark; the per-row book is entry-px based).
 */
type Row = {
  key: string;
  strategy_id: string;
  symbol: string;
  signed_qty: number;
  avg_px: number;
  realized_pnl: number;
  market_value: number;
};

const rowKey = (strategyID: string, symbol: string) => `${strategyID}:${symbol}`;

function fromSnapshot(p: LiveTradePosition): Row {
  return {
    key: rowKey(p.strategy_id, p.symbol),
    strategy_id: p.strategy_id,
    symbol: p.symbol,
    signed_qty: p.signed_qty,
    avg_px: p.avg_entry_px,
    realized_pnl: p.realized_pnl,
    market_value: p.signed_qty * p.avg_entry_px,
  };
}

function fromPushRow(p: WsLivePositionRow): Row {
  return {
    key: rowKey(p.strategy_id, p.symbol),
    strategy_id: p.strategy_id,
    symbol: p.symbol,
    signed_qty: p.signed_qty,
    avg_px: p.avg_px,
    realized_pnl: p.realized_pnl,
    market_value: p.signed_qty * p.avg_px,
  };
}

function pnlTone(v: number): string {
  return v > 0
    ? "text-emerald-600 dark:text-emerald-400"
    : v < 0
      ? "text-red-600 dark:text-red-400"
      : "text-muted-foreground";
}

/**
 * Positions panel: the open (non-flat) position book, per strategy. Hydrates
 * from PG (GET /api/v1/live/positions), then the `live_position` WS frame
 * replaces the book wholesale (it is a full snapshot, not a delta). In signal
 * mode there are never positions — the empty state is the correct, expected
 * reading, not an error.
 */
export function PositionsPanel() {
  const q = useLivePositions();
  // The latest full book pushed over WS (replace semantics), or null until one
  // arrives — then it supersedes the poll snapshot.
  const [pushed, setPushed] = useState<{ rows: Row[]; tsMs: number } | null>(
    null,
  );

  const { state } = useLiveStream({
    onLivePosition: (p: WsLivePosition) => {
      const tsMs = Math.floor(p.ts_event / 1e6);
      setPushed((prev) => {
        if (prev && prev.tsMs > tsMs) return prev; // ignore stale snapshots
        return { rows: (p.positions ?? []).map(fromPushRow), tsMs };
      });
    },
  });

  const rows = useMemo<Row[]>(() => {
    // The WS book, once present, is authoritative (it is the live full snapshot).
    const source = pushed
      ? pushed.rows
      : (q.data?.positions ?? []).map(fromSnapshot);
    return [...source].sort((a, b) =>
      a.symbol === b.symbol
        ? a.strategy_id.localeCompare(b.strategy_id)
        : a.symbol.localeCompare(b.symbol),
    );
  }, [pushed, q.data]);

  const totals = useMemo(() => {
    let mv = 0;
    let rp = 0;
    for (const r of rows) {
      mv += r.market_value;
      rp += r.realized_pnl;
    }
    return { mv, rp };
  }, [rows]);

  const noReader = q.error instanceof ApiError && q.error.status === 503;

  return (
    <Card
      data-testid="live-positions"
      data-panel="positions-panel"
      data-position-count={rows.length}
      data-connected={state === "open" ? "true" : "false"}
    >
      <CardHeader>
        <CardTitle className="text-sm">Positions</CardTitle>
        <span
          className="text-xs text-muted-foreground"
          data-testid="positions-count"
        >
          {rows.length} open {rows.length === 1 ? "position" : "positions"}
        </span>
      </CardHeader>
      <CardContent className="space-y-3">
        <DisconnectedBanner state={state} />

        {q.isLoading && !pushed ? (
          <div className="space-y-2" data-testid="positions-loading">
            <Skeleton className="h-8 w-full" />
            <Skeleton className="h-8 w-full" />
            <Skeleton className="h-8 w-full" />
          </div>
        ) : noReader ? (
          <EmptyState
            title="Live trading reader not configured"
            hint="Positions appear once a paper/live session is running. In signal mode there are no positions."
            data-testid="positions-no-reader"
          />
        ) : q.error ? (
          <ErrorState
            error={q.error}
            onRetry={() => q.refetch()}
            data-testid="positions-error"
          />
        ) : rows.length === 0 ? (
          <EmptyState
            title="No open positions"
            hint="The book is flat. Positions appear here as paper/live orders fill."
            data-testid="positions-empty"
          />
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Symbol</TableHead>
                <TableHead>Strategy</TableHead>
                <TableHead>Side</TableHead>
                <TableHead className="text-right">Qty</TableHead>
                <TableHead className="text-right">Avg px</TableHead>
                <TableHead className="text-right">Market value</TableHead>
                <TableHead className="text-right">Realized P/L</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {rows.map((r) => (
                <TableRow
                  key={r.key}
                  data-testid="live-position-row"
                  data-strategy-id={r.strategy_id}
                  data-symbol={r.symbol}
                  data-signed-qty={r.signed_qty}
                >
                  <TableCell className="font-mono font-medium">
                    {r.symbol}
                  </TableCell>
                  <TableCell className="font-mono text-xs text-muted-foreground">
                    {r.strategy_id}
                  </TableCell>
                  <TableCell>
                    <SideBadge side={r.signed_qty >= 0 ? "LONG" : "SHORT"} />
                  </TableCell>
                  <TableCell className="text-right font-mono">
                    {formatInt(Math.abs(r.signed_qty))}
                  </TableCell>
                  <TableCell className="text-right font-mono">
                    {formatMoney(r.avg_px)}
                  </TableCell>
                  <TableCell className="text-right font-mono">
                    {formatMoney(r.market_value)}
                  </TableCell>
                  <TableCell
                    className={`text-right font-mono ${pnlTone(r.realized_pnl)}`}
                    data-testid="position-realized-pnl"
                  >
                    {formatMoney(r.realized_pnl)}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
            <TableFooter>
              <TableRow data-testid="positions-totals">
                <TableCell colSpan={5} className="text-xs uppercase tracking-wide text-muted-foreground">
                  Total
                </TableCell>
                <TableCell className="text-right font-mono">
                  {formatMoney(totals.mv)}
                </TableCell>
                <TableCell
                  className={`text-right font-mono ${pnlTone(totals.rp)}`}
                >
                  {formatMoney(totals.rp)}
                </TableCell>
              </TableRow>
            </TableFooter>
          </Table>
        )}
      </CardContent>
    </Card>
  );
}
