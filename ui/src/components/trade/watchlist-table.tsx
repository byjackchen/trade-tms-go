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
import { Input } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import { IntentStateBadge } from "./live-badges";
import { DisconnectedBanner } from "./disconnected-banner";
import { cn } from "@/lib/utils";
import type { ManualSide } from "@/lib/api/types";
import { formatNum, formatRelative } from "@/lib/format";

// A signal state is "idle" (no operator action) when it is no_setup/flat/empty.
// Everything else (forming / hold / buy / sell / exit / …) is ACTIONABLE.
const isIdleState = (st?: string) => !st || st === "no_setup" || st === "flat";

// Sortable columns. "default" is the actionable-first ranking (actionable before
// idle, then strength desc, then symbol) — the most useful default for a
// multi-thousand-symbol universe.
type SortKey = "default" | "symbol" | "state" | "strategy" | "strength" | "as_of";

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

  // Filter + sort controls (client-side, over the full tracked universe). State
  // defaults to "actionable" so the no_setup tail (the bulk of a full-universe
  // SEPA scan) is collapsed by default; strategy "all"; sort = actionable-first.
  const [query, setQuery] = useState("");
  const [stateFilter, setStateFilter] = useState<string>("actionable");
  const [strategyFilter, setStrategyFilter] = useState<string>("all");
  const [sortKey, setSortKey] = useState<SortKey>("default");
  const [sortDir, setSortDir] = useState<"asc" | "desc">("desc");

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

  // Latest intent per symbol. Primary source: the watchlist payload's own
  // `intents` (the API joins the latest intent PER SYMBOL across the whole
  // tracked universe, frontier-windowed + ranked actionable-first) so every row
  // has its state. The capped `live/intents` poll and the WS `signal_intent`
  // pushes then overlay on top for freshness (latest ts wins).
  const intentBySymbol = useMemo(() => {
    const m = new Map<string, IntentInfo>();
    const merge = (
      sym: string,
      strategy_id: string,
      state: string,
      strength: number,
      ts: string,
    ) => {
      const info: IntentInfo = { strategy_id, state, strength, ts, tsMs: new Date(ts).getTime() };
      const existing = m.get(sym);
      if (!existing || info.tsMs >= existing.tsMs) m.set(sym, info);
    };
    for (const i of symbolsQ.data?.intents ?? []) {
      merge(i.symbol, i.strategy_id, i.state, i.strength, i.ts);
    }
    for (const i of intentsQ.data?.intents ?? []) {
      merge(i.symbol, i.strategy_id, i.state, i.strength, i.ts);
    }
    for (const [sym, info] of wsIntents) {
      const existing = m.get(sym);
      if (!existing || info.tsMs >= existing.tsMs) m.set(sym, info);
    }
    return m;
  }, [symbolsQ.data, intentsQ.data, wsIntents]);

  // The full tracked universe: every symbol with a latest intent, plus any
  // published-but-not-yet-fired symbol.
  const allSymbols = useMemo(() => {
    const set = new Set<string>(wsSymbols ?? symbolsQ.data?.symbols ?? []);
    for (const s of intentBySymbol.keys()) set.add(s);
    return [...set];
  }, [wsSymbols, symbolsQ.data, intentBySymbol]);

  // Strategy options actually present (for the strategy filter dropdown).
  const strategyOptions = useMemo(() => {
    const s = new Set<string>();
    for (const info of intentBySymbol.values()) if (info.strategy_id) s.add(info.strategy_id);
    return [...s].sort();
  }, [intentBySymbol]);

  // Auto-fallback for the "actionable" default: when the universe currently has
  // zero actionable signals, an empty table is unhelpful, so the effective filter
  // falls back to "all".
  const actionableCount = useMemo(
    () =>
      allSymbols.reduce(
        (n, sym) => (isIdleState(intentBySymbol.get(sym)?.state) ? n : n + 1),
        0,
      ),
    [allSymbols, intentBySymbol],
  );
  const effectiveStateFilter =
    stateFilter === "actionable" && actionableCount === 0 ? "all" : stateFilter;

  // rows: the filtered + sorted view actually rendered (and exported to CSV).
  const rows = useMemo(() => {
    const q = query.trim().toUpperCase();
    const matchState = (st?: string) =>
      effectiveStateFilter === "all"
        ? true
        : effectiveStateFilter === "actionable"
          ? !isIdleState(st)
          : (st ?? "") === effectiveStateFilter;
    const filtered = allSymbols.filter((sym) => {
      const info = intentBySymbol.get(sym);
      if (q && !sym.includes(q)) return false;
      if (strategyFilter !== "all" && info?.strategy_id !== strategyFilter) return false;
      return matchState(info?.state);
    });

    const dir = sortDir === "asc" ? 1 : -1;
    const byDefault = (a: string, b: string) => {
      const ia = intentBySymbol.get(a);
      const ib = intentBySymbol.get(b);
      const aa = ia && !isIdleState(ia.state) ? 0 : 1;
      const ba = ib && !isIdleState(ib.state) ? 0 : 1;
      if (aa !== ba) return aa - ba;
      const sa = ia?.strength ?? -Infinity;
      const sb = ib?.strength ?? -Infinity;
      if (aa === 0 && sa !== sb) return sb - sa;
      return a.localeCompare(b);
    };
    const cmp = (a: string, b: string) => {
      if (sortKey === "default") return byDefault(a, b);
      const ia = intentBySymbol.get(a);
      const ib = intentBySymbol.get(b);
      let r = 0;
      switch (sortKey) {
        case "symbol":
          r = a.localeCompare(b);
          break;
        case "state":
          r = (ia?.state ?? "").localeCompare(ib?.state ?? "");
          break;
        case "strategy":
          r = (ia?.strategy_id ?? "").localeCompare(ib?.strategy_id ?? "");
          break;
        case "strength":
          r = (ia?.strength ?? -Infinity) - (ib?.strength ?? -Infinity);
          break;
        case "as_of":
          r = (ia?.tsMs ?? -Infinity) - (ib?.tsMs ?? -Infinity);
          break;
      }
      return r !== 0 ? r * dir : a.localeCompare(b);
    };
    return filtered.sort(cmp).map((sym) => ({ sym, info: intentBySymbol.get(sym) }));
  }, [allSymbols, intentBySymbol, query, effectiveStateFilter, strategyFilter, sortKey, sortDir]);

  // Toggle a column sort: same column flips direction; a new column resets to a
  // sensible default (numeric desc, text asc).
  const toggleSort = (key: SortKey) => {
    if (key === sortKey) {
      setSortDir((d) => (d === "asc" ? "desc" : "asc"));
    } else {
      setSortKey(key);
      setSortDir(key === "strength" || key === "as_of" ? "desc" : "asc");
    }
  };

  const noReader =
    symbolsQ.error instanceof ApiError && symbolsQ.error.status === 503;

  // downloadCsv exports the exact rows currently shown — i.e. the filtered +
  // sorted view, not the whole universe — client-side, no backend call. Header
  // columns match the table.
  const downloadCsv = () => {
    const header = ["symbol", "latest_state", "strategy", "strength", "as_of"];
    const lines = [header.join(",")];
    for (const { sym, info } of rows) {
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

  // sortHead renders a clickable, sort-indicating column header.
  const sortHead = (k: SortKey, label: string, align: "left" | "right" = "left") => (
    <TableHead className={align === "right" ? "text-right" : undefined}>
      <button
        type="button"
        onClick={() => toggleSort(k)}
        data-testid={`watchlist-sort-${k}`}
        className={cn(
          "inline-flex items-center gap-1 transition-colors hover:text-foreground",
          align === "right" && "flex-row-reverse",
          sortKey === k ? "text-foreground" : "text-muted-foreground",
        )}
      >
        {label}
        <span className="text-[10px] leading-none">
          {sortKey === k ? (sortDir === "asc" ? "▲" : "▼") : "↕"}
        </span>
      </button>
    </TableHead>
  );

  const clearFilters = () => {
    setQuery("");
    setStateFilter("all");
    setStrategyFilter("all");
  };

  return (
    <Card data-testid="live-watchlist" data-panel="watchlist-table">
      <CardHeader className="flex flex-row items-center justify-between gap-2">
        <div className="flex items-baseline gap-2">
          <CardTitle className="text-sm">Watchlist</CardTitle>
          <span className="text-xs text-muted-foreground" data-testid="watchlist-count">
            {rows.length === allSymbols.length
              ? `${allSymbols.length} ${allSymbols.length === 1 ? "symbol" : "symbols"}`
              : `${rows.length} of ${allSymbols.length}`}
          </span>
        </div>
        <Button
          variant="outline"
          size="sm"
          onClick={downloadCsv}
          disabled={rows.length === 0}
          data-testid="watchlist-download"
          title="Download the filtered watchlist as CSV"
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
        ) : allSymbols.length === 0 ? (
          <EmptyState
            title="Empty watchlist"
            hint="Symbols appear here as sessions emit intents for them."
            data-testid="watchlist-empty"
          />
        ) : (
          <>
            {/* Filter toolbar: symbol search + state + strategy. Defaults to
                actionable so the no_setup tail is collapsed; "All states" shows
                the full universe. */}
            <div className="flex flex-wrap items-center gap-2" data-testid="watchlist-filters">
              <Input
                type="search"
                placeholder="Search symbol…"
                value={query}
                onChange={(e) => setQuery(e.target.value)}
                className="h-8 w-40"
                data-testid="watchlist-search"
                aria-label="Search symbol"
              />
              <Select
                value={stateFilter}
                onChange={(e) => setStateFilter(e.target.value)}
                className="w-36"
                data-testid="watchlist-state-filter"
                aria-label="Filter by state"
              >
                <option value="actionable">Actionable</option>
                <option value="all">All states</option>
                <option value="forming">forming</option>
                <option value="hold">hold</option>
                <option value="buy">buy</option>
                <option value="sell">sell</option>
                <option value="exit">exit</option>
                <option value="no_setup">no_setup</option>
              </Select>
              <Select
                value={strategyFilter}
                onChange={(e) => setStrategyFilter(e.target.value)}
                className="w-40"
                data-testid="watchlist-strategy-filter"
                aria-label="Filter by strategy"
              >
                <option value="all">All strategies</option>
                {strategyOptions.map((s) => (
                  <option key={s} value={s}>
                    {s}
                  </option>
                ))}
              </Select>
              {effectiveStateFilter !== stateFilter ? (
                <span className="text-xs text-muted-foreground">
                  no actionable signals — showing all
                </span>
              ) : null}
            </div>

            {rows.length === 0 ? (
              <div className="flex flex-col items-center gap-3" data-testid="watchlist-no-match">
                <EmptyState
                  title="No symbols match the filters"
                  hint="Adjust the search / state / strategy filters."
                />
                <Button
                  variant="outline"
                  size="sm"
                  onClick={clearFilters}
                  data-testid="watchlist-clear-filters"
                >
                  Clear filters
                </Button>
              </div>
            ) : (
              <Table>
                <TableHeader>
                  <TableRow>
                    {sortHead("symbol", "Symbol")}
                    {sortHead("state", "Latest state")}
                    {sortHead("strategy", "Strategy")}
                    {sortHead("strength", "Strength", "right")}
                    {sortHead("as_of", "As of", "right")}
                    <TableHead className="text-right">Trade</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {rows.map(({ sym, info }) => (
                    <TableRow key={sym} data-testid="live-watchlist-row" data-symbol={sym}>
                      <TableCell className="font-mono font-medium">{sym}</TableCell>
                      <TableCell>
                        {info ? (
                          <IntentStateBadge state={info.state} />
                        ) : (
                          <span className="text-xs text-muted-foreground">tracking</span>
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
                          href={`/trade/desk?symbol=${encodeURIComponent(sym)}&side=${suggestedSide(info?.state)}`}
                          data-testid="manual-trade-from-signal"
                          data-symbol={sym}
                          data-side={suggestedSide(info?.state)}
                          className={cn(buttonVariants({ variant: "outline", size: "sm" }))}
                        >
                          Trade
                        </Link>
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            )}
          </>
        )}
      </CardContent>
    </Card>
  );
}
