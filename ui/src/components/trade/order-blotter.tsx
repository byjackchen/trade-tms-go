"use client";

import { useEffect, useMemo, useState } from "react";
import { useLiveOrders } from "@/lib/api/hooks";
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
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Skeleton } from "@/components/ui/skeleton";
import { EmptyState, ErrorState } from "@/components/shell/states";
import { DisconnectedBanner } from "./disconnected-banner";
import { OrderStatusBadge, SideBadge } from "./live-badges";
import type {
  LiveOrder,
  LiveOrderStatus,
  WsOrderUpdate,
} from "@/lib/api/types";
import { formatInt, formatMoney, formatRelative } from "@/lib/format";

/**
 * A normalized order row keyed by client_order_id (the idempotency key — one row
 * per logical order, its latest state machine status wins). This mirrors the
 * server's idempotent-submission contract: a reconnect/retry never double-counts.
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
 * Order blotter: the submitted orders with their state-machine status (ADR-004)
 * and timestamps. Hydrates from PG (GET /api/v1/live/orders, newest-first), then
 * `order_update` WS frames advance each order's status in place — keyed by
 * client_order_id so a state transition replaces, never appends.
 */
export function OrderBlotter({ accountId }: { accountId?: string } = {}) {
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

  return (
    // `live-blotter` (root) + `live-blotter-order-row` is the e2e contract
    // (spec 24/26). `live-orders` is kept as a back-compat alias selector.
    <Card
      data-testid="live-blotter"
      data-orders="live-orders"
      data-panel="order-blotter"
      data-order-count={rows.length}
      data-connected={state === "open" ? "true" : "false"}
    >
      <CardHeader>
        <CardTitle className="text-sm">Order blotter</CardTitle>
        <span
          className="text-xs text-muted-foreground"
          data-testid="orders-count"
        >
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
            hint="Orders appear once a paper/live session submits them. In signal mode no orders are ever sent."
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
            hint="Submitted orders appear here with their live state-machine status."
            data-testid="orders-empty"
          />
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Symbol</TableHead>
                <TableHead>Strategy</TableHead>
                <TableHead>Side</TableHead>
                <TableHead className="text-right">Qty</TableHead>
                <TableHead className="text-right">Filled</TableHead>
                <TableHead className="text-right">Avg px</TableHead>
                <TableHead>Status</TableHead>
                <TableHead className="text-right">As of</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {rows.map((r) => (
                <TableRow
                  key={r.client_order_id}
                  data-testid="live-blotter-order-row"
                  data-client-order-id={r.client_order_id}
                  data-symbol={r.symbol}
                  data-status={String(r.status).toUpperCase()}
                  data-filled-qty={r.filled_qty}
                >
                  <TableCell className="font-mono font-medium">
                    {r.symbol}
                  </TableCell>
                  <TableCell className="font-mono text-xs text-muted-foreground">
                    {r.strategy_id}
                  </TableCell>
                  <TableCell>
                    <SideBadge side={r.side} />
                  </TableCell>
                  <TableCell className="text-right font-mono">
                    {formatInt(r.qty)}
                  </TableCell>
                  <TableCell className="text-right font-mono">
                    {formatInt(r.filled_qty)}
                  </TableCell>
                  <TableCell className="text-right font-mono">
                    {r.avg_fill_px ? formatMoney(r.avg_fill_px) : "—"}
                  </TableCell>
                  <TableCell>
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
                  </TableCell>
                  <TableCell
                    className="text-right text-xs text-muted-foreground"
                    title={r.ts}
                  >
                    {formatRelative(r.ts, now)}
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
