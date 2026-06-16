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
import { DisconnectedBanner } from "../disconnected-banner";
import { OrderStatusBadge, SideBadge } from "../live-badges";
import {
  MANUAL_STRATEGY_ID,
  type LiveOrder,
  type LiveOrderStatus,
  type WsOrderUpdate,
} from "@/lib/api/types";
import { formatInt, formatMoney, formatRelative } from "@/lib/format";

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
 * Manual-desk ORDER BLOTTER: manual + auto orders with their state-machine status,
 * live over WS. Hydrates from PG (GET /api/v1/live/orders), then `order_update`
 * frames advance each order in place (keyed by client_order_id — idempotent).
 *
 * Per-row CANCEL on WORKING manual orders (POST /api/v1/trade/order/{coid}/cancel).
 * Only MANUAL-attributed orders are cancellable from here. A wire build without the
 * modify-order proto returns 501 `cancel_unsupported`; we surface that truthfully
 * (`manual-cancel-unsupported`) and NEVER imply the working real order was
 * cancelled.
 */
export function TradeBlotter({ accountId }: { accountId?: string } = {}) {
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
      if (!existing || r.tsMs >= existing.tsMs) merged.set(r.client_order_id, r);
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

  return (
    <Card
      data-testid="manual-blotter"
      data-panel="manual-blotter"
      data-order-count={rows.length}
      data-connected={state === "open" ? "true" : "false"}
    >
      <CardHeader>
        <CardTitle className="text-sm">Order blotter</CardTitle>
        <span className="text-xs text-muted-foreground" data-testid="manual-orders-count">
          {rows.length} {rows.length === 1 ? "order" : "orders"} (manual + auto)
        </span>
      </CardHeader>
      <CardContent className="space-y-3">
        <DisconnectedBanner state={state} />

        {unsupported ? (
          <Alert variant="warning" data-testid="manual-cancel-unsupported">
            <AlertDescription>
              Cancel is not supported on this broker build — the working order was
              NOT cancelled. {unsupported}
            </AlertDescription>
          </Alert>
        ) : null}

        {cancelError ? (
          <Alert variant="destructive" data-testid="manual-cancel-error">
            <AlertDescription>{cancelError}</AlertDescription>
          </Alert>
        ) : null}

        {q.isLoading ? (
          <div className="space-y-2" data-testid="manual-orders-loading">
            <Skeleton className="h-8 w-full" />
            <Skeleton className="h-8 w-full" />
          </div>
        ) : noReader ? (
          <EmptyState
            title="Live trading reader not configured"
            hint="Orders appear once a paper/live (or manual) session submits them."
            data-testid="manual-orders-no-reader"
          />
        ) : q.error ? (
          <ErrorState
            error={q.error}
            onRetry={() => q.refetch()}
            data-testid="manual-orders-error"
          />
        ) : rows.length === 0 ? (
          <EmptyState
            title="No orders yet"
            hint="Submitted manual + auto orders appear here with their live status."
            data-testid="manual-orders-empty"
          />
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Symbol</TableHead>
                <TableHead>Book</TableHead>
                <TableHead>Side</TableHead>
                <TableHead className="text-right">Qty</TableHead>
                <TableHead className="text-right">Filled</TableHead>
                <TableHead className="text-right">Avg px</TableHead>
                <TableHead>Status</TableHead>
                <TableHead className="text-right">As of</TableHead>
                <TableHead className="text-right">Action</TableHead>
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
                    data-testid="manual-blotter-order-row"
                    data-client-order-id={r.client_order_id}
                    data-symbol={r.symbol}
                    data-status={String(r.status).toUpperCase()}
                    data-filled-qty={r.filled_qty}
                    data-manual={manual ? "true" : "false"}
                  >
                    <TableCell className="font-mono font-medium">
                      {r.symbol}
                    </TableCell>
                    <TableCell>
                      {manual ? (
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
