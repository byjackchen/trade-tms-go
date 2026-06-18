"use client";

import { useMemo, useState } from "react";
import { useLivePositions } from "@/lib/api/hooks";
import { useLiveStream } from "@/lib/api/use-live-stream";
import { ApiError } from "@/lib/api/client";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import {
  ResponsiveTable,
  type ColumnDef,
} from "@/components/ui/responsive-table";
import { Skeleton } from "@/components/ui/skeleton";
import { Badge } from "@/components/ui/badge";
import { EmptyState, ErrorState } from "@/components/shell/states";
import { DisconnectedBanner } from "./disconnected-banner";
import { SideBadge } from "./live-badges";
import {
  EXTERNAL_STRATEGY_ID,
  type LiveTradePosition,
  type WsLivePosition,
  type WsLivePositionRow,
} from "@/lib/api/types";
import { formatInt, formatMoney } from "@/lib/format";

/**
 * A normalized position row keyed by (strategy_id, symbol). Market value uses
 * the average entry price as the mark when no live quote is on the wire.
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
 * The Account view's read-only positions table — the account's open book.
 *
 * It surfaces every strategy book PLUS the EXTERNAL book (positions placed
 * directly at the broker and pulled back in by "Sync from broker"). EXTERNAL rows
 * are badged so the operator can tell the synced external book from the auto
 * strategies'. There is NO order ENTRY here — orders are placed at the broker.
 *
 * Hydrates from PG (GET /api/v1/trade/positions), then the `live_position` WS
 * frame replaces the book wholesale (a full snapshot, not a delta).
 */
export function PositionsTable({
  accountId,
}: {
  accountId?: string;
} = {}) {
  const q = useLivePositions(accountId);

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

  // Column definitions drive both the desktop table and the mobile card list
  // (full feature set — every column ports). Symbol + Side are `primary` so each
  // mobile card leads with the at-a-glance line; the rest fold under "More".
  const columns: ColumnDef<Row>[] = [
    {
      key: "symbol",
      header: "Symbol",
      primary: true,
      render: (r) => <span className="font-mono font-medium">{r.symbol}</span>,
    },
    {
      key: "book",
      header: "Book",
      render: (r) =>
        r.strategy_id === EXTERNAL_STRATEGY_ID ? (
          <Badge variant="secondary" data-testid="position-external-badge">
            EXTERNAL
          </Badge>
        ) : (
          <span className="font-mono text-xs text-muted-foreground">
            {r.strategy_id}
          </span>
        ),
    },
    {
      key: "side",
      header: "Side",
      primary: true,
      render: (r) => <SideBadge side={r.signed_qty >= 0 ? "LONG" : "SHORT"} />,
    },
    {
      key: "qty",
      header: "Qty",
      align: "right",
      render: (r) => (
        <span className="font-mono">{formatInt(Math.abs(r.signed_qty))}</span>
      ),
    },
    {
      key: "avg_px",
      header: "Avg px",
      align: "right",
      render: (r) => <span className="font-mono">{formatMoney(r.avg_px)}</span>,
    },
    {
      key: "market_value",
      header: "Market value",
      align: "right",
      render: (r) => (
        <span className="font-mono">{formatMoney(r.market_value)}</span>
      ),
    },
    {
      key: "realized_pnl",
      header: "Realized P/L",
      align: "right",
      render: (r) => (
        <span
          className={`font-mono ${pnlTone(r.realized_pnl)}`}
          data-testid="position-realized-pnl"
        >
          {formatMoney(r.realized_pnl)}
        </span>
      ),
    },
  ];

  return (
    <Card
      data-testid="live-positions"
      data-panel="positions-panel"
      data-position-count={rows.length}
      data-connected={state === "open" ? "true" : "false"}
    >
      <CardHeader>
        <CardTitle className="text-sm">Positions</CardTitle>
        <span className="text-xs text-muted-foreground" data-testid="positions-count">
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
            hint="Positions appear once a paper/live session is running, or after a broker sync. In signal mode there are no auto positions."
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
            hint="The book is flat. Positions appear here as paper/live orders fill, or after you sync externally-placed positions from the broker."
            data-testid="positions-empty"
          />
        ) : (
          <>
            <ResponsiveTable<Row>
              columns={columns}
              rows={rows}
              rowKey={(r) => r.key}
              rowTestId={() => "live-position-row"}
              rowAttrs={(r) => ({
                "data-strategy-id": r.strategy_id,
                "data-symbol": r.symbol,
                "data-signed-qty": String(r.signed_qty),
                "data-external":
                  r.strategy_id === EXTERNAL_STRATEGY_ID ? "true" : "false",
              })}
              data-testid="positions-responsive-table"
            />
            {/* Totals — kept as a dedicated strip below the table so it works in
                BOTH surfaces (desktop table + mobile card list) and preserves the
                `…-totals` e2e contract with the MV / realized-P&L values. */}
            <div
              data-testid="positions-totals"
              className="flex items-center justify-between gap-3 border-t pt-2 text-sm"
            >
              <span className="text-xs uppercase tracking-wide text-muted-foreground">
                Total
              </span>
              <span className="flex items-center gap-6">
                <span className="flex flex-col items-end">
                  <span className="text-[10px] uppercase tracking-wide text-muted-foreground">
                    Market value
                  </span>
                  <span className="font-mono">{formatMoney(totals.mv)}</span>
                </span>
                <span className="flex flex-col items-end">
                  <span className="text-[10px] uppercase tracking-wide text-muted-foreground">
                    Realized P/L
                  </span>
                  <span className={`font-mono ${pnlTone(totals.rp)}`}>
                    {formatMoney(totals.rp)}
                  </span>
                </span>
              </span>
            </div>
          </>
        )}
      </CardContent>
    </Card>
  );
}
