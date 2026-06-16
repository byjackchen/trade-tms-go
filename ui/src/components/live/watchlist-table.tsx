"use client";

import { useEffect, useMemo, useState } from "react";
import Link from "next/link";
import { useWatchlist, useLiveIntents } from "@/lib/api/hooks";
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
import { EmptyState, ErrorState } from "@/components/shell/states";
import { Button, buttonVariants } from "@/components/ui/button";
import { IntentStateBadge } from "./live-badges";
import { DisconnectedBanner } from "./disconnected-banner";
import { cn } from "@/lib/utils";
import type { ManualSide } from "@/lib/api/types";
import { formatNum, formatRelative } from "@/lib/format";

/**
 * Map a signal's decision state to the suggested MANUAL order side for the
 * trade-from-signal flow: a bullish entry (buy / long / enter) suggests BUY; an
 * exit / bearish state (exit / sell / short / flat) suggests SELL. The operator
 * always reviews + can flip the side in the ticket before submitting.
 */
function suggestedSide(state: string | undefined): ManualSide {
  const s = (state ?? "").toLowerCase();
  if (s === "exit" || s === "sell" || s === "short" || s === "flat") return "SELL";
  return "BUY";
}

// csvCell quotes a value per RFC 4180: wrap in double-quotes and double any
// embedded quote when the value contains a quote, comma, or newline.
function csvCell(v: string | number): string {
  const s = String(v);
  return /[",\n]/.test(s) ? `"${s.replace(/"/g, '""')}"` : s;
}

type IntentInfo = {
  strategy_id: string;
  state: string;
  strength: number;
  ts: string;
  tsMs: number;
};

/**
 * Watchlist: the live universe (symbols the recent sessions emitted intents
 * for) joined with the latest intent per symbol. Symbols stream in via the
 * `watchlist` WS frame; per-symbol intents via the `signal_intent` frame. The
 * union is the tracked universe even before any intent fires for a symbol.
 */
export function WatchlistTable() {
  const symbolsQ = useWatchlist();
  const intentsQ = useLiveIntents();
  const [wsSymbols, setWsSymbols] = useState<string[] | null>(null);
  const [wsIntents, setWsIntents] = useState<Map<string, IntentInfo>>(new Map());
  const [now, setNow] = useState(() => Date.now());

  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), 5000);
    return () => clearInterval(id);
  }, []);

  const { state } = useLiveStream({
    onWatchlist: (p) => setWsSymbols(p.symbols ?? []),
    onSignalIntent: (p) => {
      const intent = p.intent_json ?? {};
      const info: IntentInfo = {
        strategy_id: p.strategy_id,
        state: String(intent["state"] ?? intent["signal"] ?? "—"),
        strength: Number(intent["strength"] ?? intent["score"] ?? 0) || 0,
        ts: new Date(Math.floor(p.ts_event / 1e6)).toISOString(),
        tsMs: Math.floor(p.ts_event / 1e6),
      };
      setWsIntents((prev) => {
        const next = new Map(prev);
        const existing = next.get(p.symbol);
        if (!existing || info.tsMs >= existing.tsMs) next.set(p.symbol, info);
        return next;
      });
    },
  });

  // Latest intent per symbol from the poll, then overlaid by WS pushes.
  const intentBySymbol = useMemo(() => {
    const m = new Map<string, IntentInfo>();
    for (const i of intentsQ.data?.intents ?? []) {
      const info: IntentInfo = {
        strategy_id: i.strategy_id,
        state: i.state,
        strength: i.strength,
        ts: i.ts,
        tsMs: new Date(i.ts).getTime(),
      };
      const existing = m.get(i.symbol);
      if (!existing || info.tsMs >= existing.tsMs) m.set(i.symbol, info);
    }
    for (const [sym, info] of wsIntents) {
      const existing = m.get(sym);
      if (!existing || info.tsMs >= existing.tsMs) m.set(sym, info);
    }
    return m;
  }, [intentsQ.data, wsIntents]);

  const symbols = useMemo(() => {
    const set = new Set<string>(wsSymbols ?? symbolsQ.data?.symbols ?? []);
    // Any symbol that has an intent but isn't in the published list still belongs.
    for (const s of intentBySymbol.keys()) set.add(s);
    return [...set].sort();
  }, [wsSymbols, symbolsQ.data, intentBySymbol]);

  const noReader =
    symbolsQ.error instanceof ApiError && symbolsQ.error.status === 503;

  // downloadCsv exports the exact rows currently shown (symbol + latest intent),
  // client-side, no backend call. Header columns match the table.
  const downloadCsv = () => {
    const header = ["symbol", "latest_state", "strategy", "strength", "as_of"];
    const lines = [header.join(",")];
    for (const sym of symbols) {
      const info = intentBySymbol.get(sym);
      lines.push(
        [
          csvCell(sym),
          csvCell(info?.state ?? ""),
          csvCell(info?.strategy_id ?? ""),
          csvCell(info ? info.strength : ""),
          csvCell(info?.ts ?? ""),
        ].join(","),
      );
    }
    const blob = new Blob([lines.join("\n") + "\n"], {
      type: "text/csv;charset=utf-8",
    });
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = `watchlist-${new Date().toISOString().replace(/[:.]/g, "-")}.csv`;
    document.body.appendChild(a);
    a.click();
    a.remove();
    URL.revokeObjectURL(url);
  };

  return (
    <Card data-testid="live-watchlist" data-panel="watchlist-table">
      <CardHeader className="flex flex-row items-center justify-between gap-2">
        <div className="flex items-baseline gap-2">
          <CardTitle className="text-sm">Watchlist</CardTitle>
          <span className="text-xs text-muted-foreground" data-testid="watchlist-count">
            {symbols.length} {symbols.length === 1 ? "symbol" : "symbols"}
          </span>
        </div>
        <Button
          variant="outline"
          size="sm"
          onClick={downloadCsv}
          disabled={symbols.length === 0}
          data-testid="watchlist-download"
          title="Download the current watchlist as CSV"
        >
          Download CSV
        </Button>
      </CardHeader>
      <CardContent className="space-y-3">
        <DisconnectedBanner state={state} />
        {symbolsQ.isLoading ? (
          <div className="space-y-2" data-testid="watchlist-loading">
            <Skeleton className="h-8 w-full" />
            <Skeleton className="h-8 w-full" />
          </div>
        ) : noReader ? (
          <EmptyState
            title="Live reader not configured"
            hint="The tracked universe appears once a signal session runs."
            data-testid="watchlist-no-reader"
          />
        ) : symbolsQ.error ? (
          <ErrorState
            error={symbolsQ.error}
            onRetry={() => symbolsQ.refetch()}
            data-testid="watchlist-error"
          />
        ) : symbols.length === 0 ? (
          <EmptyState
            title="Empty watchlist"
            hint="Symbols appear here as sessions emit intents for them."
            data-testid="watchlist-empty"
          />
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Symbol</TableHead>
                <TableHead>Latest state</TableHead>
                <TableHead>Strategy</TableHead>
                <TableHead className="text-right">Strength</TableHead>
                <TableHead className="text-right">As of</TableHead>
                <TableHead className="text-right">Trade</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {symbols.map((sym) => {
                const info = intentBySymbol.get(sym);
                return (
                  <TableRow key={sym} data-testid="live-watchlist-row" data-symbol={sym}>
                    <TableCell className="font-mono font-medium">{sym}</TableCell>
                    <TableCell>
                      {info ? (
                        <IntentStateBadge state={info.state} />
                      ) : (
                        <span className="text-xs text-muted-foreground">
                          tracking
                        </span>
                      )}
                    </TableCell>
                    <TableCell className="font-mono text-xs text-muted-foreground">
                      {info?.strategy_id ?? "—"}
                    </TableCell>
                    <TableCell className="text-right font-mono">
                      {info ? formatNum(info.strength, 1) : "—"}
                    </TableCell>
                    <TableCell
                      className="text-right text-xs text-muted-foreground"
                      title={info?.ts}
                    >
                      {info ? formatRelative(info.ts, now) : "—"}
                    </TableCell>
                    <TableCell className="text-right">
                      {/* Trade-from-signal: pre-fill the manual order ticket with
                          the symbol + a side suggested by the signal state. The
                          operator reviews + confirms on the trade desk. */}
                      <Link
                        href={`/live/desk?symbol=${encodeURIComponent(sym)}&side=${suggestedSide(info?.state)}`}
                        data-testid="manual-trade-from-signal"
                        data-symbol={sym}
                        data-side={suggestedSide(info?.state)}
                        className={cn(
                          buttonVariants({ variant: "outline", size: "sm" }),
                        )}
                      >
                        Trade
                      </Link>
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
