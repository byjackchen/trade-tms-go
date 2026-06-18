"use client";

import { useEffect, useMemo, useState } from "react";
import { useLiveOrders } from "@/lib/api/hooks";
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
import { OrderStatusBadge, SideBadge } from "./live-badges";
import {
  EXTERNAL_STRATEGY_ID,
  type LiveOrder,
  type LiveOrderStatus,
  type WsOrderUpdate,
} from "@/lib/api/types";
import { formatInt, formatMoney, formatRelative } from "@/lib/format";

/**
 * A normalized order row keyed by client_order_id (the idempotency key — one row
 * per logical order, its latest state machine status wins).
 */
type Row = {
  client_order_id: string;
  venue_order_id?: string;
  strategy_id: string;
  symbol: string;
  side: string;
  qty: number;
  filled_qty: number;
  avg_fill_px: number;
  status: LiveOrderStatus;
  reason?: string;
  ts: string;
  tsMs: number;
};

function fromOrder(o: LiveOrder): Row {
  return {
    client_order_id: o.client_order_id,
    venue_order_id: o.venue_order_id,
    strategy_id: o.strategy_id,
    symbol: o.symbol,
    side: o.side,
    qty: o.qty,
    filled_qty: o.filled_qty,
    avg_fill_px: o.avg_fill_px,
    status: o.status,
    reason: o.reason,
    ts: o.ts,
    tsMs: new Date(o.ts).getTime(),
  };
}

function fromPush(p: WsOrderUpdate): Row {
  const tsMs = Math.floor(p.ts_event / 1e6);
  return {
    client_order_id: p.client_order_id,
    venue_order_id: p.venue_order_id,
    strategy_id: p.strategy_id,
    symbol: p.symbol,
    side: p.side,
    qty: p.qty,
    filled_qty: p.filled_qty,
    avg_fill_px: p.avg_fill_px,
    status: p.status,
    reason: p.reason,
    ts: new Date(tsMs).toISOString(),
    tsMs,
  };
}

/**
 * The Account view's read-only order blotter — the account's recent order
 * activity across every strategy book PLUS the EXTERNAL book (orders placed
 * directly at the broker and pulled back in by "Sync from broker"). EXTERNAL rows
 * are badged. There is NO cancel here — orders are managed at the broker.
 *
 * Hydrates from PG (GET /api/v1/trade/orders, newest-first), then `order_update`
 * frames advance each order in place — keyed by client_order_id (idempotent).
 */
export function Blotter({
  accountId,
}: {
  accountId?: string;
} = {}) {
  const q = useLiveOrders(undefined, accountId);
  const [pushed, setPushed] = useState<Map<string, Row>>(new Map());
  const [now, setNow] = useState(() => Date.now());

  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), 5000);
    return () => clearInterval(id);
  }, []);

  const { state } = useLiveStream({
    onOrderUpdate: (p) => {
      const row = fromPush(p);
      setPushed((prev) => {
        const next = new Map(prev);
        const existing = next.get(row.client_order_id);
        // Newest event-time wins so out-of-order frames don't regress status.
        if (!existing || row.tsMs >= existing.tsMs)
          next.set(row.client_order_id, row);
        return next;
      });
    },
  });

  const rows = useMemo(() => {
    const merged = new Map<string, Row>();
    for (const o of q.data?.orders ?? []) {
      const r = fromOrder(o);
      const existing = merged.get(r.client_order_id);
      if (!existing || r.tsMs >= existing.tsMs)
        merged.set(r.client_order_id, r);
    }
    for (const [k, r] of pushed) {
      const existing = merged.get(k);
      if (!existing || r.tsMs >= existing.tsMs) merged.set(k, r);
    }
    return [...merged.values()].sort((a, b) => b.tsMs - a.tsMs);
  }, [q.data, pushed]);

  const noReader = q.error instanceof ApiError && q.error.status === 503;

  // Column defs drive desktop table + mobile card list (full feature set). Symbol +
  // Status lead each mobile card.
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
          <Badge variant="secondary" data-testid="order-external-badge">
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
      render: (r) => <SideBadge side={r.side} />,
    },
    {
      key: "qty",
      header: "Qty",
      align: "right",
      render: (r) => <span className="font-mono">{formatInt(r.qty)}</span>,
    },
    {
      key: "filled",
      header: "Filled",
      align: "right",
      render: (r) => <span className="font-mono">{formatInt(r.filled_qty)}</span>,
    },
    {
      key: "avg_px",
      header: "Avg px",
      align: "right",
      render: (r) => (
        <span className="font-mono">
          {r.avg_fill_px ? formatMoney(r.avg_fill_px) : "—"}
        </span>
      ),
    },
    {
      key: "status",
      header: "Status",
      primary: true,
      render: (r) => (
        <span className="flex items-center gap-1.5">
          <OrderStatusBadge status={r.status} />
          {r.reason ? (
            <span
              className="max-w-[10rem] truncate text-xs text-muted-foreground"
              title={r.reason}
            >
              {r.reason}
            </span>
          ) : null}
        </span>
      ),
    },
    {
      key: "asof",
      header: "As of",
      align: "right",
      render: (r) => (
        <span className="text-xs text-muted-foreground" title={r.ts}>
          {formatRelative(r.ts, now)}
        </span>
      ),
    },
  ];

  return (
    <Card
      data-testid="live-blotter"
      data-orders="live-orders"
      data-panel="order-blotter"
      data-order-count={rows.length}
      data-connected={state === "open" ? "true" : "false"}
    >
      <CardHeader>
        <CardTitle className="text-sm">Order blotter</CardTitle>
        <span className="text-xs text-muted-foreground" data-testid="orders-count">
          {rows.length} {rows.length === 1 ? "order" : "orders"}
        </span>
      </CardHeader>
      <CardContent className="space-y-3">
        <DisconnectedBanner state={state} />

        {q.isLoading ? (
          <div className="space-y-2" data-testid="orders-loading">
            <Skeleton className="h-8 w-full" />
            <Skeleton className="h-8 w-full" />
            <Skeleton className="h-8 w-full" />
          </div>
        ) : noReader ? (
          <EmptyState
            title="Live trading reader not configured"
            hint="Orders appear once a paper/live session submits them, or after a broker sync. In signal mode no orders are ever sent."
            data-testid="orders-no-reader"
          />
        ) : q.error ? (
          <ErrorState
            error={q.error}
            onRetry={() => q.refetch()}
            data-testid="orders-error"
          />
        ) : rows.length === 0 ? (
          <EmptyState
            title="No orders yet"
            hint="Submitted orders appear here with their live state-machine status. Externally-placed orders appear after a broker sync."
            data-testid="orders-empty"
          />
        ) : (
          <ResponsiveTable<Row>
            columns={columns}
            rows={rows}
            rowKey={(r) => r.client_order_id}
            rowTestId={() => "live-blotter-order-row"}
            rowAttrs={(r) => ({
              "data-client-order-id": r.client_order_id,
              "data-symbol": r.symbol,
              "data-status": String(r.status).toUpperCase(),
              "data-filled-qty": String(r.filled_qty),
              "data-external":
                r.strategy_id === EXTERNAL_STRATEGY_ID ? "true" : "false",
            })}
            data-testid="blotter-responsive-table"
          />
        )}
      </CardContent>
    </Card>
  );
}
