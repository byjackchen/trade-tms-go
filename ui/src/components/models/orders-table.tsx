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
          <Table data-testid="orders-table">
            <TableHeader>
              <TableRow>
                <TableHead>Order</TableHead>
                <TableHead>Instrument</TableHead>
                <TableHead>Side</TableHead>
                <TableHead>Type</TableHead>
                <TableHead className="text-right">Qty</TableHead>
                <TableHead className="text-right">Price</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Submitted</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {orders.map((o, i) => {
                const oid = orderId(o, i);
                const status = pick(o, ["status"]);
                const ts = pick(o, ["ts_init", "ts_last"]);
                return (
                  <TableRow key={oid} data-testid={`order-row-${i}`}>
                    <TableCell
                      className="max-w-[12rem] truncate font-mono text-xs"
                      title={oid}
                      data-testid="order-id"
                    >
                      {oid}
                    </TableCell>
                    <TableCell className="font-mono text-xs">
                      {pick(o, ["instrument_id", "symbol"])}
                    </TableCell>
                    <TableCell>
                      <Badge variant="outline">{pick(o, ["side"])}</Badge>
                    </TableCell>
                    <TableCell className="text-xs">
                      {pick(o, ["order_type", "type"])}
                    </TableCell>
                    <TableCell className="text-right tabular-nums">
                      {pick(o, ["quantity", "qty"])}
                    </TableCell>
                    <TableCell className="text-right tabular-nums">
                      {pick(o, ["avg_px", "price"])}
                    </TableCell>
                    <TableCell data-testid="order-status">
                      <Badge variant={statusVariant(status)}>{status}</Badge>
                    </TableCell>
                    <TableCell
                      className="text-xs text-muted-foreground"
                      title={ts !== "—" ? formatTs(ts) : undefined}
                    >
                      {ts !== "—" ? ts.slice(0, 10) : "—"}
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
