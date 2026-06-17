"use client";

import { useEffect, useMemo, useState } from "react";
import { useLiveOrders, useCancelManualOrder } from "@/lib/api/hooks";
import { useLiveStream } from "@/lib/api/use-live-stream";
import { ApiError } from "@/lib/api/client";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Skeleton } from "@/components/ui/skeleton";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { EmptyState, ErrorState } from "@/components/shell/states";
import { DisconnectedBanner } from "./disconnected-banner";
import { OrderStatusBadge, SideBadge } from "./live-badges";
import {
  MANUAL_STRATEGY_ID,
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

/** Working = the order is live at the venue and can still be cancelled. */
const WORKING: ReadonlySet<string> = new Set([
  "SUBMITTED",
  "ACCEPTED",
  "WORKING",
  "PARTIALLY_FILLED",
]);

function isWorking(status: LiveOrderStatus): boolean {
  return WORKING.has(String(status).toUpperCase());
}

/**
 * THE shared order blotter — one component for both the Portfolio view (read-only) and
 * the manual desk (acting).
 *
 * `withActions=false` (Portfolio view): a read-only blotter — the recent-activity
 * OVERVIEW. Emits the `live-blotter` / `live-blotter-order-row` contract.
 *
 * `withActions=true` (desk): a MANUAL/auto book column + per-row CANCEL on
 * WORKING manual orders (POST /api/v1/trade/order/{coid}/cancel). A wire build
 * without the modify-order proto returns 501; we surface that truthfully and
 * NEVER imply the working order was cancelled. Emits the `manual-blotter` /
 * `manual-blotter-order-row` contract.
 *
 * Hydrates from PG (GET /api/v1/trade/orders, newest-first), then `order_update`
 * frames advance each order in place — keyed by client_order_id (idempotent).
 */
export function Blotter({
  withActions = false,
  accountId,
}: {
  withActions?: boolean;
  accountId?: string;
} = {}) {
  const q = useLiveOrders(undefined, accountId);
  const cancel = useCancelManualOrder();
  const [pushed, setPushed] = useState<Map<string, Row>>(new Map());
  const [now, setNow] = useState(() => Date.now());
  const [cancelling, setCancelling] = useState<string | null>(null);
  // Sticky "cancel not supported on this build" banner (501) — truthful messaging.
  const [unsupported, setUnsupported] = useState<string | null>(null);

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

  // A non-501 cancel failure (e.g. 400) surfaces inline; 501 has its own banner.
  const cancelError =
    cancel.error instanceof ApiError && cancel.error.status !== 501
      ? `${cancel.error.code}: ${cancel.error.message}`
      : null;

  function onCancel(coid: string) {
    setCancelling(coid);
    setUnsupported(null);
    cancel.mutate(coid, {
      onError: (err) => {
        if (err instanceof ApiError && err.status === 501) {
          setUnsupported(
            `${coid}: ${err.message || "cancel is not supported on this broker build"}`,
          );
        }
      },
      onSettled: () => setCancelling(null),
    });
  }

  // Testid prefix keeps the e2e contract: Portfolio view `live-blotter`, desk
  // `manual-blotter`.
  const rootId = withActions ? "manual-blotter" : "live-blotter";
  const rowId = withActions ? "manual-blotter-order-row" : "live-blotter-order-row";
  const countId = withActions ? "manual-orders-count" : "orders-count";

  return (
    <Card
      data-testid={rootId}
      data-orders={withActions ? undefined : "live-orders"}
      data-panel={withActions ? "manual-blotter" : "order-blotter"}
      data-order-count={rows.length}
      data-connected={state === "open" ? "true" : "false"}
    >
      <CardHeader>
        <CardTitle className="text-sm">Order blotter</CardTitle>
        <span className="text-xs text-muted-foreground" data-testid={countId}>
          {rows.length} {rows.length === 1 ? "order" : "orders"}
          {withActions ? " (manual + auto)" : ""}
        </span>
      </CardHeader>
      <CardContent className="space-y-3">
        <DisconnectedBanner state={state} />

        {withActions && unsupported ? (
          <Alert variant="warning" data-testid="manual-cancel-unsupported">
            <AlertDescription>
              Cancel is not supported on this broker build — the working order was
              NOT cancelled. {unsupported}
            </AlertDescription>
          </Alert>
        ) : null}

        {withActions && cancelError ? (
          <Alert variant="destructive" data-testid="manual-cancel-error">
            <AlertDescription>{cancelError}</AlertDescription>
          </Alert>
        ) : null}

        {q.isLoading ? (
          <div
            className="space-y-2"
            data-testid={withActions ? "manual-orders-loading" : "orders-loading"}
          >
            <Skeleton className="h-8 w-full" />
            <Skeleton className="h-8 w-full" />
            <Skeleton className="h-8 w-full" />
          </div>
        ) : noReader ? (
          <EmptyState
            title="Live trading reader not configured"
            hint="Orders appear once a paper/live session submits them. In signal mode no orders are ever sent."
            data-testid={withActions ? "manual-orders-no-reader" : "orders-no-reader"}
          />
        ) : q.error ? (
          <ErrorState
            error={q.error}
            onRetry={() => q.refetch()}
            data-testid={withActions ? "manual-orders-error" : "orders-error"}
          />
        ) : rows.length === 0 ? (
          <EmptyState
            title="No orders yet"
            hint={
              withActions
                ? "Submitted manual + auto orders appear here with their live status."
                : "Submitted orders appear here with their live state-machine status."
            }
            data-testid={withActions ? "manual-orders-empty" : "orders-empty"}
          />
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Symbol</TableHead>
                <TableHead>{withActions ? "Book" : "Strategy"}</TableHead>
                <TableHead>Side</TableHead>
                <TableHead className="text-right">Qty</TableHead>
                <TableHead className="text-right">Filled</TableHead>
                <TableHead className="text-right">Avg px</TableHead>
                <TableHead>Status</TableHead>
                <TableHead className="text-right">As of</TableHead>
                {withActions ? (
                  <TableHead className="text-right">Action</TableHead>
                ) : null}
              </TableRow>
            </TableHeader>
            <TableBody>
              {rows.map((r) => {
                const manual = r.strategy_id === MANUAL_STRATEGY_ID;
                const working = isWorking(r.status);
                const pending = cancelling === r.client_order_id;
                return (
                  <TableRow
                    key={r.client_order_id}
                    data-testid={rowId}
                    data-client-order-id={r.client_order_id}
                    data-symbol={r.symbol}
                    data-status={String(r.status).toUpperCase()}
                    data-filled-qty={r.filled_qty}
                    data-manual={withActions ? (manual ? "true" : "false") : undefined}
                  >
                    <TableCell className="font-mono font-medium">
                      {r.symbol}
                    </TableCell>
                    <TableCell>
                      {withActions && manual ? (
                        <Badge variant="secondary" data-testid="order-manual-badge">
                          MANUAL
                        </Badge>
                      ) : (
                        <span className="font-mono text-xs text-muted-foreground">
                          {r.strategy_id}
                        </span>
                      )}
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
                    {withActions ? (
                      <TableCell className="text-right">
                        {manual && working ? (
                          <Button
                            variant="outline"
                            size="sm"
                            disabled={pending}
                            onClick={() => onCancel(r.client_order_id)}
                            data-testid="manual-order-cancel"
                            data-client-order-id={r.client_order_id}
                          >
                            {pending ? "Cancelling…" : "Cancel"}
                          </Button>
                        ) : (
                          <span className="text-xs text-muted-foreground">—</span>
                        )}
                      </TableCell>
                    ) : null}
                  </TableRow>
                );
              })}
            </TableBody>
          </Table>
        )}
      </CardContent>
    </Card>
  );
}
