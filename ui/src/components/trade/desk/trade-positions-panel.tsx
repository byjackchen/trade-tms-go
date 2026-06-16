"use client";

import { useMemo, useState } from "react";
import { useLivePositions, useCloseManualPosition } from "@/lib/api/hooks";
import { useLiveStream } from "@/lib/api/use-live-stream";
import { ApiError } from "@/lib/api/client";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
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
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Dialog } from "@/components/ui/dialog";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { EmptyState, ErrorState } from "@/components/shell/states";
import { DisconnectedBanner } from "../disconnected-banner";
import { SideBadge } from "../live-badges";
import { useTradeDesk } from "./trade-desk-context";
import {
  MANUAL_LIVE_CONFIRM_PHRASE,
  MANUAL_STRATEGY_ID,
  type LiveTradePosition,
  type ManualSide,
  type WsLivePosition,
  type WsLivePositionRow,
} from "@/lib/api/types";
import { formatInt, formatMoney } from "@/lib/format";

type Row = {
  key: string;
  strategy_id: string;
  symbol: string;
  signed_qty: number;
  avg_px: number;
  realized_pnl: number;
  market_value: number;
};

const rowKey = (s: string, sym: string) => `${s}:${sym}`;

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
 * Manual-desk POSITIONS panel: the full open book (every strategy) so the
 * operator sees the whole picture, with per-row actions:
 *   - Trade — pre-fills the order ticket with a flattening side (long → SELL,
 *     short → BUY) for a discretionary trim/add.
 *   - Close — flattens the symbol's MANUAL position via
 *     POST /api/v1/trade/position/{symbol}/close, behind a confirmation that
 *     requires the trade password (paper) or the exact live phrase (LIVE-armed).
 *     A close bypasses the allocator budget. Only MANUAL-attributed rows are
 *     closable here — auto books are the strategy's to manage (the flatten cmd).
 */
export function TradePositionsPanel({
  liveArmed,
  accountId,
}: {
  liveArmed: boolean;
  accountId?: string;
}) {
  const q = useLivePositions(accountId);
  const close = useCloseManualPosition();
  const desk = useTradeDesk();

  const [pushed, setPushed] = useState<{ rows: Row[]; tsMs: number } | null>(
    null,
  );
  const { state } = useLiveStream({
    onLivePosition: (p: WsLivePosition) => {
      const tsMs = Math.floor(p.ts_event / 1e6);
      setPushed((prev) => {
        if (prev && prev.tsMs > tsMs) return prev;
        return { rows: (p.positions ?? []).map(fromPushRow), tsMs };
      });
    },
  });

  // Close dialog state.
  const [closeSym, setCloseSym] = useState<string | null>(null);
  const [closeQty, setCloseQty] = useState("");
  const [confirmToken, setConfirmToken] = useState("");

  const rows = useMemo<Row[]>(() => {
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

  function openClose(symbol: string) {
    setCloseSym(symbol);
    setCloseQty("");
    setConfirmToken("");
    close.reset();
  }
  function closeDialog() {
    setCloseSym(null);
  }

  function submitClose() {
    if (!closeSym) return;
    const qtyNum = Number(closeQty);
    const partial = closeQty.trim() !== "" && Number.isFinite(qtyNum) && qtyNum > 0;
    close.mutate(
      {
        symbol: closeSym,
        body: {
          ...(partial ? { qty: Math.floor(qtyNum) } : {}),
          confirm_token: confirmToken.trim(),
        },
      },
      { onSuccess: () => closeDialog() },
    );
  }

  // Side that REDUCES a position (for the Trade prefill): long → SELL, short → BUY.
  const flatteningSide = (signed: number): ManualSide =>
    signed >= 0 ? "SELL" : "BUY";

  // The close confirm input is always required (paper password / live phrase);
  // when LIVE-armed it must match the phrase exactly before submit is enabled.
  const tokenPresent = confirmToken.trim().length > 0;
  const tokenOk = liveArmed
    ? confirmToken.trim() === MANUAL_LIVE_CONFIRM_PHRASE
    : tokenPresent;
  const canClose = tokenOk && !close.isPending;

  return (
    <Card
      data-testid="manual-positions"
      data-panel="manual-positions"
      data-position-count={rows.length}
      data-connected={state === "open" ? "true" : "false"}
    >
      <CardHeader>
        <CardTitle className="text-sm">Positions</CardTitle>
        <span className="text-xs text-muted-foreground" data-testid="manual-positions-count">
          {rows.length} open {rows.length === 1 ? "position" : "positions"}
        </span>
      </CardHeader>
      <CardContent className="space-y-3">
        <DisconnectedBanner state={state} />

        {q.isLoading && !pushed ? (
          <div className="space-y-2" data-testid="manual-positions-loading">
            <Skeleton className="h-8 w-full" />
            <Skeleton className="h-8 w-full" />
          </div>
        ) : noReader ? (
          <EmptyState
            title="Live trading reader not configured"
            hint="Positions appear once a paper/live (or manual) session is running."
            data-testid="manual-positions-no-reader"
          />
        ) : q.error ? (
          <ErrorState
            error={q.error}
            onRetry={() => q.refetch()}
            data-testid="manual-positions-error"
          />
        ) : rows.length === 0 ? (
          <EmptyState
            title="No open positions"
            hint="The book is flat. Place an order in the ticket to open one."
            data-testid="manual-positions-empty"
          />
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Symbol</TableHead>
                <TableHead>Book</TableHead>
                <TableHead>Side</TableHead>
                <TableHead className="text-right">Qty</TableHead>
                <TableHead className="text-right">Avg px</TableHead>
                <TableHead className="text-right">Market value</TableHead>
                <TableHead className="text-right">Realized P/L</TableHead>
                <TableHead className="text-right">Actions</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {rows.map((r) => {
                const isManual = r.strategy_id === MANUAL_STRATEGY_ID;
                return (
                  <TableRow
                    key={r.key}
                    data-testid="manual-position-row"
                    data-strategy-id={r.strategy_id}
                    data-symbol={r.symbol}
                    data-signed-qty={r.signed_qty}
                    data-manual={isManual ? "true" : "false"}
                  >
                    <TableCell className="font-mono font-medium">
                      {r.symbol}
                    </TableCell>
                    <TableCell>
                      {isManual ? (
                        <Badge variant="secondary" data-testid="position-manual-badge">
                          MANUAL
                        </Badge>
                      ) : (
                        <span className="font-mono text-xs text-muted-foreground">
                          {r.strategy_id}
                        </span>
                      )}
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
                    >
                      {formatMoney(r.realized_pnl)}
                    </TableCell>
                    <TableCell className="text-right">
                      <span className="flex justify-end gap-1.5">
                        <Button
                          variant="outline"
                          size="sm"
                          onClick={() =>
                            desk.requestTrade(r.symbol, flatteningSide(r.signed_qty))
                          }
                          data-testid="position-trade-button"
                        >
                          Trade
                        </Button>
                        <Button
                          variant="destructive"
                          size="sm"
                          onClick={() => openClose(r.symbol)}
                          disabled={!isManual || r.signed_qty === 0}
                          title={
                            isManual
                              ? "Close the MANUAL position in this symbol"
                              : "Only MANUAL positions are closable here"
                          }
                          data-testid="manual-position-close"
                          data-symbol={r.symbol}
                        >
                          Close
                        </Button>
                      </span>
                    </TableCell>
                  </TableRow>
                );
              })}
            </TableBody>
            <TableFooter>
              <TableRow data-testid="manual-positions-totals">
                <TableCell
                  colSpan={5}
                  className="text-xs uppercase tracking-wide text-muted-foreground"
                >
                  Total
                </TableCell>
                <TableCell className="text-right font-mono">
                  {formatMoney(totals.mv)}
                </TableCell>
                <TableCell className={`text-right font-mono ${pnlTone(totals.rp)}`}>
                  {formatMoney(totals.rp)}
                </TableCell>
                <TableCell />
              </TableRow>
            </TableFooter>
          </Table>
        )}
      </CardContent>

      {/* ---- Close confirmation dialog ---- */}
      <Dialog
        open={closeSym !== null}
        onClose={closeDialog}
        data-testid="manual-close-confirm"
        title={
          <span>
            Close MANUAL position —{" "}
            <span className="font-mono">{closeSym}</span>
          </span>
        }
        description={
          liveArmed ? (
            <span className="block font-medium text-destructive">
              LIVE (real-money) close — type the confirmation phrase. A close
              bypasses the allocator budget and is allowed even under a halt.
            </span>
          ) : (
            "Submits a market order that flattens the MANUAL net in this symbol. Leave quantity blank for a full close; a positive quantity partial-closes (clamped to the open size). Enter the trade password to authorize."
          )
        }
        footer={
          <>
            <Button
              variant="outline"
              onClick={closeDialog}
              disabled={close.isPending}
              data-testid="manual-close-confirm-cancel"
            >
              Cancel
            </Button>
            <Button
              variant="destructive"
              disabled={!canClose}
              aria-disabled={!canClose}
              onClick={submitClose}
              data-testid="manual-close-confirm-submit"
            >
              {close.isPending ? "Closing…" : "Close position"}
            </Button>
          </>
        }
      >
        <div className="space-y-4">
          <div className="space-y-1.5">
            <Label htmlFor="close-qty">Quantity (blank = full close)</Label>
            <Input
              id="close-qty"
              type="number"
              min={1}
              step={1}
              inputMode="numeric"
              value={closeQty}
              onChange={(e) => setCloseQty(e.target.value)}
              placeholder="full close"
              className="font-mono"
              data-testid="manual-close-qty"
            />
          </div>

          <div className="space-y-1.5">
            <Label htmlFor="close-confirm">
              {liveArmed ? (
                <>
                  Confirm phrase —{" "}
                  <span className="rounded bg-muted px-1 font-mono text-foreground">
                    {MANUAL_LIVE_CONFIRM_PHRASE}
                  </span>
                </>
              ) : (
                "Trade password (confirm_token)"
              )}
            </Label>
            <Input
              id="close-confirm"
              type={liveArmed ? "text" : "password"}
              value={confirmToken}
              onChange={(e) => setConfirmToken(e.target.value)}
              placeholder={
                liveArmed ? MANUAL_LIVE_CONFIRM_PHRASE : "paper trade password"
              }
              autoComplete="off"
              data-testid="manual-close-confirm-input"
            />
          </div>

          {close.error instanceof ApiError ? (
            <Alert variant="destructive" data-testid="manual-close-error">
              <AlertDescription>{close.error.message}</AlertDescription>
            </Alert>
          ) : null}
        </div>
      </Dialog>
    </Card>
  );
}
