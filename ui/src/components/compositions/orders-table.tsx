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
import { useBacktestOrders } from "@/lib/api/hooks";
import { formatTs } from "@/lib/format";
import type { BacktestOrder } from "@/lib/api/types";

/** First defined string-ish field, coerced to a display string. */
function pick(o: BacktestOrder, keys: string[]): string {
  for (const k of keys) {
    const v = o[k];
    if (v !== undefined && v !== null && v !== "") return String(v);
  }
  return "—";
}

function orderId(o: BacktestOrder, i: number): string {
  const v = pick(o, ["client_order_id", "order_id"]);
  return v === "—" ? `idx-${i}` : v;
}

function statusVariant(
  s: string,
): "success" | "destructive" | "secondary" | "muted" {
  const u = s.toUpperCase();
  if (u.includes("FILL")) return "success";
  if (u.includes("REJECT") || u.includes("DENIED") || u.includes("CANCEL"))
    return "destructive";
  if (u === "—") return "muted";
  return "secondary";
}

/** Column definitions for the orders ResponsiveTable. Order id + Status are the
 * mobile-card primaries; the rest (full parity) fold under "More". */
const COLUMNS: ColumnDef<BacktestOrder>[] = [
  {
    key: "order",
    header: "Order",
    primary: true,
    className: "max-w-[12rem] truncate font-mono text-xs",
    render: (o, i) => {
      const oid = orderId(o, i);
      return (
        <span title={oid} data-testid="order-id">
          {oid}
        </span>
      );
    },
  },
  {
    key: "instrument",
    header: "Instrument",
    className: "font-mono text-xs",
    render: (o) => pick(o, ["instrument_id", "symbol"]),
  },
  {
    key: "side",
    header: "Side",
    render: (o) => <Badge variant="outline">{pick(o, ["side"])}</Badge>,
  },
  {
    key: "type",
    header: "Type",
    className: "text-xs",
    render: (o) => pick(o, ["order_type", "type"]),
  },
  {
    key: "qty",
    header: "Qty",
    align: "right",
    className: "tabular-nums",
    render: (o) => pick(o, ["quantity", "qty"]),
  },
  {
    key: "price",
    header: "Price",
    align: "right",
    className: "tabular-nums",
    render: (o) => pick(o, ["avg_px", "price"]),
  },
  {
    key: "status",
    header: "Status",
    primary: true,
    render: (o) => {
      const status = pick(o, ["status"]);
      return (
        <span data-testid="order-status">
          <Badge variant={statusVariant(status)}>{status}</Badge>
        </span>
      );
    },
  },
  {
    key: "submitted",
    header: "Submitted",
    className: "text-xs text-muted-foreground",
    render: (o) => {
      const ts = pick(o, ["ts_init", "ts_last"]);
      return (
        <span title={ts !== "—" ? formatTs(ts) : undefined}>
          {ts !== "—" ? ts.slice(0, 10) : "—"}
        </span>
      );
    },
  },
];

export function OrdersTable({ id }: { id: number }) {
  const { data, isLoading, error, refetch } = useBacktestOrders(id);
  const orders: BacktestOrder[] = data ?? [];

  return (
    <Card data-testid="orders-card">
      <CardHeader className="flex-col items-start gap-1">
        <CardTitle>Orders</CardTitle>
        <CardDescription>
          Submitted orders (engine pass-through), in submission order.
        </CardDescription>
      </CardHeader>
      <CardContent>
        {isLoading ? (
          <LoadingRows rows={4} data-testid="orders-loading" />
        ) : error ? (
          <ErrorState
            error={error}
            onRetry={() => refetch()}
            data-testid="orders-error"
          />
        ) : orders.length === 0 ? (
          <EmptyState
            title="No orders"
            hint="This run submitted no orders."
            data-testid="orders-empty"
          />
        ) : (
          <ResponsiveTable
            columns={COLUMNS}
            rows={orders}
            rowKey={(o, i) => orderId(o, i)}
            rowTestId={(_o, i) => `order-row-${i}`}
            data-testid="orders-table"
          />
        )}
      </CardContent>
    </Card>
  );
}
