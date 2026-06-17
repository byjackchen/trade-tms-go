"use client";

import { useEffect, useMemo, useState } from "react";
import { useLiveFills } from "@/lib/api/hooks";
import { useLiveStream } from "@/lib/api/use-live-stream";
import { ApiError } from "@/lib/api/client";
import {
  Card,
  CardContent,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  ResponsiveTable,
  type ColumnDef,
} from "@/components/ui/responsive-table";
import { Skeleton } from "@/components/ui/skeleton";
import { EmptyState, ErrorState } from "@/components/shell/states";
import { DisconnectedBanner } from "./disconnected-banner";
import type { LiveFill, WsFillUpdate } from "@/lib/api/types";
import { formatInt, formatMoney, formatRelative } from "@/lib/format";

/**
 * A fill row keyed by trade_id. Fills are append-only executions (distinct
 * trade_ids), unlike orders (whose status mutates in place). The WS frame
 * prepends new executions; the trade_id de-dupes a poll/push overlap.
 */
type Row = {
  trade_id: string;
  symbol: string;
  qty: number;
  price: number;
  commission: number;
  ts: string;
  tsMs: number;
};

function fromFill(f: LiveFill): Row {
  return {
    trade_id: f.trade_id,
    symbol: f.symbol,
    qty: f.qty,
    price: f.price,
    commission: f.commission,
    ts: f.ts,
    tsMs: new Date(f.ts).getTime(),
  };
}

function fromPush(p: WsFillUpdate): Row {
  const tsMs = Math.floor(p.ts_event / 1e6);
  return {
    trade_id: p.trade_id,
    symbol: p.symbol,
    qty: p.qty,
    price: p.price,
    commission: p.commission,
    ts: new Date(tsMs).toISOString(),
    tsMs,
  };
}

/**
 * Fills list: the execution tape (newest first). Hydrates from PG
 * (GET /api/v1/live/fills), then `fill_update` WS frames prepend new
 * executions live. Capped at 200 rows in the view (the tape can be long; the
 * DB stays the full record).
 */
export function FillsList({ accountId }: { accountId?: string } = {}) {
  const q = useLiveFills(undefined, accountId);
  const [pushed, setPushed] = useState<Map<string, Row>>(new Map());
  const [now, setNow] = useState(() => Date.now());

  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), 5000);
    return () => clearInterval(id);
  }, []);

  const { state } = useLiveStream({
    onFillUpdate: (p) => {
      const row = fromPush(p);
      setPushed((prev) => {
        if (prev.has(row.trade_id)) return prev;
        const next = new Map(prev);
        next.set(row.trade_id, row);
        return next;
      });
    },
  });

  const rows = useMemo(() => {
    const merged = new Map<string, Row>();
    for (const f of q.data?.fills ?? []) merged.set(f.trade_id, fromFill(f));
    for (const [k, r] of pushed) if (!merged.has(k)) merged.set(k, r);
    return [...merged.values()].sort((a, b) => b.tsMs - a.tsMs).slice(0, 200);
  }, [q.data, pushed]);

  const noReader = q.error instanceof ApiError && q.error.status === 503;

  return (
    <Card
      data-testid="live-fills"
      data-panel="fills-list"
      data-fill-count={rows.length}
      data-connected={state === "open" ? "true" : "false"}
    >
      <CardHeader>
        <CardTitle className="text-sm">Fills</CardTitle>
        <span
          className="text-xs text-muted-foreground"
          data-testid="fills-count"
        >
          {rows.length} {rows.length === 1 ? "fill" : "fills"}
        </span>
      </CardHeader>
      <CardContent className="space-y-3">
        <DisconnectedBanner state={state} />

        {q.isLoading ? (
          <div className="space-y-2" data-testid="fills-loading">
            <Skeleton className="h-8 w-full" />
            <Skeleton className="h-8 w-full" />
          </div>
        ) : noReader ? (
          <EmptyState
            title="Live trading reader not configured"
            hint="Executions appear once paper/live orders fill."
            data-testid="fills-no-reader"
          />
        ) : q.error ? (
          <ErrorState
            error={q.error}
            onRetry={() => q.refetch()}
            data-testid="fills-error"
          />
        ) : rows.length === 0 ? (
          <EmptyState
            title="No fills yet"
            hint="The execution tape is empty. Fills appear here as orders execute."
            data-testid="fills-empty"
          />
        ) : (
          <ResponsiveTable<Row>
            columns={[
              {
                key: "symbol",
                header: "Symbol",
                primary: true,
                render: (r) => (
                  <span className="font-mono font-medium">{r.symbol}</span>
                ),
              },
              {
                key: "qty",
                header: "Qty",
                align: "right",
                primary: true,
                render: (r) => (
                  <span className="font-mono">{formatInt(r.qty)}</span>
                ),
              },
              {
                key: "price",
                header: "Price",
                align: "right",
                primary: true,
                render: (r) => (
                  <span className="font-mono">{formatMoney(r.price)}</span>
                ),
              },
              {
                key: "commission",
                header: "Commission",
                align: "right",
                render: (r) => (
                  <span className="font-mono text-muted-foreground">
                    {formatMoney(r.commission)}
                  </span>
                ),
              },
              {
                key: "trade_id",
                header: "Trade id",
                render: (r) => (
                  <span
                    className="block max-w-[12rem] truncate font-mono text-xs text-muted-foreground"
                    title={r.trade_id}
                  >
                    {r.trade_id}
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
            ]}
            rows={rows}
            rowKey={(r) => r.trade_id}
            rowTestId={() => "live-fill-row"}
            rowAttrs={(r) => ({
              "data-trade-id": r.trade_id,
              "data-symbol": r.symbol,
            })}
            data-testid="fills-responsive-table"
          />
        )}
      </CardContent>
    </Card>
  );
}
