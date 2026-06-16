"use client";

import { useMemo, useState } from "react";
import Link from "next/link";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Skeleton } from "@/components/ui/skeleton";
import { Button, buttonVariants } from "@/components/ui/button";
import { EmptyState, ErrorState } from "@/components/shell/states";
import { cn } from "@/lib/utils";
import { formatNum, formatMoney } from "@/lib/format";
import {
  useStrategyIntents,
  num,
  bool,
  type StrategyIntentRow,
} from "./use-strategy-intents";
import { SortHead, csvCell, downloadCsv } from "./shared";

/**
 * SEPA tab — the ACTIONABLE trade-plan table.
 *
 * Columns: Symbol | %→Pivot | Pivot | Stop | Risk% | RS | %off 52wk-hi |
 * Base (depth/age) | Vol | Readiness. Sorted DESC by buy_readiness by default;
 * every metric column is sortable (Base + the trailing action column are plain,
 * non-sortable headers). The "buy zone" (small positive proximity, 0..2%) is
 * highlighted. The saturated strength=100 is DE-EMPHASIZED — we render "8/8" (a
 * trend-template pass) instead of "100.0". Missing fields show "—".
 */

type SepaSortKey =
  | "symbol"
  | "proximity"
  | "pivot"
  | "stop"
  | "risk"
  | "rs"
  | "off_high"
  | "vol"
  | "readiness";

// A row is in the "buy zone" when it is just below / at the pivot (a small
// positive proximity_to_trigger_pct, here 0..2%).
function inBuyZone(prox: number | null): boolean {
  return prox != null && prox >= 0 && prox <= 2;
}

// Suggested side for the Trade prefill: SEPA is long-only; an exit-ish state
// suggests SELL, everything else BUY.
function suggestedSide(state: string): "BUY" | "SELL" {
  const s = state.toLowerCase();
  return s === "exit" || s === "sell" ? "SELL" : "BUY";
}

export function SepaTable({
  symbolFilter,
  accountId,
}: {
  symbolFilter: string;
  accountId?: string;
}) {
  const { rows, isLoading, error, noReader, refetch } = useStrategyIntents(
    "sepa",
    { symbolFilter },
  );
  const [sortKey, setSortKey] = useState<SepaSortKey>("readiness");
  const [sortDir, setSortDir] = useState<"asc" | "desc">("desc");

  const onSort = (k: SepaSortKey) => {
    if (k === sortKey) setSortDir((d) => (d === "asc" ? "desc" : "asc"));
    else {
      setSortKey(k);
      setSortDir(k === "symbol" ? "asc" : "desc");
    }
  };

  const sorted = useMemo(() => {
    const dir = sortDir === "asc" ? 1 : -1;
    const val = (r: StrategyIntentRow, k: SepaSortKey): number | string => {
      const i = r.intent;
      switch (k) {
        case "symbol":
          return r.symbol;
        case "proximity":
          return num(i, "proximity_to_trigger_pct") ?? -Infinity;
        case "pivot":
          return num(i, "pivot_price") ?? -Infinity;
        case "stop":
          return num(i, "stop_price") ?? -Infinity;
        case "risk":
          return num(i, "risk_pct") ?? -Infinity;
        case "rs":
          return num(i, "rs_rank") ?? -Infinity;
        case "off_high":
          return num(i, "pct_off_52wk_high") ?? -Infinity;
        case "vol":
          return num(i, "vol_ratio") ?? -Infinity;
        case "readiness":
          return num(i, "buy_readiness") ?? -Infinity;
      }
    };
    return [...rows].sort((a, b) => {
      const va = val(a, sortKey);
      const vb = val(b, sortKey);
      let r: number;
      if (typeof va === "string" || typeof vb === "string")
        r = String(va).localeCompare(String(vb));
      else r = va - vb;
      return r !== 0 ? r * dir : a.symbol.localeCompare(b.symbol);
    });
  }, [rows, sortKey, sortDir]);

  const exportCsv = () => {
    const header = [
      "symbol",
      "pct_to_pivot",
      "pivot",
      "stop",
      "risk_pct",
      "rs_rank",
      "pct_off_52wk_high",
      "base_depth_pct",
      "base_age_days",
      "vol_ratio",
      "buy_readiness",
      "trend_template_pass",
    ];
    const lines = [header.join(",")];
    for (const r of sorted) {
      const i = r.intent;
      lines.push(
        [
          csvCell(r.symbol),
          csvCell(num(i, "proximity_to_trigger_pct") ?? ""),
          csvCell(num(i, "pivot_price") ?? ""),
          csvCell(num(i, "stop_price") ?? ""),
          csvCell(num(i, "risk_pct") ?? ""),
          csvCell(num(i, "rs_rank") ?? ""),
          csvCell(num(i, "pct_off_52wk_high") ?? ""),
          csvCell(num(i, "base_depth_pct") ?? ""),
          csvCell(num(i, "base_age_days") ?? ""),
          csvCell(num(i, "vol_ratio") ?? ""),
          csvCell(num(i, "buy_readiness") ?? ""),
          csvCell(bool(i, "trend_template_pass") === true ? "yes" : ""),
        ].join(","),
      );
    }
    downloadCsv("sepa-watchlist", lines);
  };

  if (isLoading) {
    return (
      <div className="space-y-2" data-testid="sepa-loading">
        <Skeleton className="h-8 w-full" />
        <Skeleton className="h-8 w-full" />
      </div>
    );
  }
  if (noReader) {
    return (
      <EmptyState
        title="Live reader not configured"
        hint="SEPA setups appear once a signal session runs."
        data-testid="sepa-no-reader"
      />
    );
  }
  if (error) {
    return <ErrorState error={error} onRetry={refetch} data-testid="sepa-error" />;
  }
  if (sorted.length === 0) {
    return (
      <EmptyState
        title="No SEPA setups"
        hint="Forming / actionable SEPA setups appear here as the scan runs."
        data-testid="sepa-empty"
      />
    );
  }

  return (
    <div className="space-y-3" data-testid="sepa-table" data-row-count={sorted.length}>
      <div className="flex items-center justify-between gap-2">
        <span className="text-xs text-muted-foreground" data-testid="sepa-count">
          {sorted.length} {sorted.length === 1 ? "setup" : "setups"}
        </span>
        <Button
          variant="outline"
          size="sm"
          onClick={exportCsv}
          data-testid="watchlist-download"
          title="Download the SEPA trade-plan as CSV"
        >
          Download CSV
        </Button>
      </div>
      <Table>
        <TableHeader>
          <TableRow>
            <SortHead k="symbol" label="Symbol" sortKey={sortKey} sortDir={sortDir} onSort={onSort} />
            <SortHead k="proximity" label="%→Pivot" sortKey={sortKey} sortDir={sortDir} onSort={onSort} align="right" title="Signed distance to the pivot (buy zone: 0..2%)" />
            <SortHead k="pivot" label="Pivot" sortKey={sortKey} sortDir={sortDir} onSort={onSort} align="right" />
            <SortHead k="stop" label="Stop" sortKey={sortKey} sortDir={sortDir} onSort={onSort} align="right" />
            <SortHead k="risk" label="Risk%" sortKey={sortKey} sortDir={sortDir} onSort={onSort} align="right" />
            <SortHead k="rs" label="RS" sortKey={sortKey} sortDir={sortDir} onSort={onSort} align="right" title="Relative-strength rank (1..99)" />
            <SortHead k="off_high" label="%off 52wk-hi" sortKey={sortKey} sortDir={sortDir} onSort={onSort} align="right" />
            <TableHead title="Base depth% / age (days)">Base</TableHead>
            <SortHead k="vol" label="Vol" sortKey={sortKey} sortDir={sortDir} onSort={onSort} align="right" title="Volume ratio vs average" />
            <SortHead k="readiness" label="Readiness" sortKey={sortKey} sortDir={sortDir} onSort={onSort} align="right" title="Buy-readiness score (0..100)" />
            <TableHead className="text-right" />
          </TableRow>
        </TableHeader>
        <TableBody>
          {sorted.map((r) => {
            const i = r.intent;
            const prox = num(i, "proximity_to_trigger_pct");
            const pivot = num(i, "pivot_price");
            const stop = num(i, "stop_price");
            const risk = num(i, "risk_pct");
            const rs = num(i, "rs_rank");
            const offHigh = num(i, "pct_off_52wk_high");
            const depth = num(i, "base_depth_pct");
            const age = num(i, "base_age_days");
            const vol = num(i, "vol_ratio");
            const readiness = num(i, "buy_readiness");
            const ttPass = bool(i, "trend_template_pass") === true;
            const buyZone = inBuyZone(prox);

            // Base column: "depth% · age d" or "—" when neither is computed.
            const base =
              depth == null && age == null
                ? "—"
                : `${depth != null ? `${formatNum(depth, 1)}%` : "—"} · ${age != null ? `${Math.round(age)}d` : "—"}`;

            return (
              <TableRow
                key={r.symbol}
                data-testid="live-watchlist-row"
                data-symbol={r.symbol}
                data-buy-zone={buyZone ? "true" : "false"}
                data-readiness={readiness ?? ""}
                className={cn(
                  buyZone &&
                    "bg-emerald-500/10 hover:bg-emerald-500/15 dark:bg-emerald-500/10",
                )}
              >
                <TableCell className="font-mono font-medium">
                  <span className="flex items-center gap-1.5">
                    {r.symbol}
                    {buyZone ? (
                      <span
                        className="rounded bg-emerald-500/20 px-1 text-[10px] font-semibold uppercase text-emerald-700 dark:text-emerald-300"
                        data-testid="sepa-buy-zone-tag"
                      >
                        buy zone
                      </span>
                    ) : null}
                  </span>
                </TableCell>
                <TableCell
                  className={cn(
                    "text-right font-mono",
                    buyZone && "font-semibold text-emerald-700 dark:text-emerald-300",
                  )}
                >
                  {prox != null ? `${prox > 0 ? "+" : ""}${formatNum(prox, 1)}%` : "—"}
                </TableCell>
                <TableCell className="text-right font-mono">
                  {pivot != null ? formatMoney(pivot) : "—"}
                </TableCell>
                <TableCell className="text-right font-mono text-muted-foreground">
                  {stop != null ? formatMoney(stop) : "—"}
                </TableCell>
                <TableCell className="text-right font-mono">
                  {risk != null ? `${formatNum(risk, 1)}%` : "—"}
                </TableCell>
                <TableCell className="text-right font-mono">
                  {rs != null ? Math.round(rs) : "—"}
                </TableCell>
                <TableCell className="text-right font-mono">
                  {offHigh != null ? `${formatNum(offHigh, 1)}%` : "—"}
                </TableCell>
                <TableCell className="font-mono text-xs text-muted-foreground">
                  {base}
                </TableCell>
                <TableCell className="text-right font-mono">
                  {vol != null ? `${formatNum(vol, 2)}×` : "—"}
                </TableCell>
                <TableCell className="text-right">
                  {/* Buy-readiness, the default sort. The saturated 8/8 trend-
                      template pass is shown as a de-emphasized check, NOT 100.0. */}
                  <span className="flex items-center justify-end gap-1.5">
                    <span className="font-mono font-medium">
                      {readiness != null ? formatNum(readiness, 0) : "—"}
                    </span>
                    {ttPass ? (
                      <span
                        className="rounded bg-muted px-1 text-[10px] text-muted-foreground"
                        title="8/8 trend-template pass"
                        data-testid="sepa-trend-template"
                      >
                        8/8 ✓
                      </span>
                    ) : null}
                  </span>
                </TableCell>
                <TableCell className="text-right">
                  <Link
                    href={`/trade/desk?symbol=${encodeURIComponent(r.symbol)}&side=${suggestedSide(r.state)}${accountId ? `&account=${encodeURIComponent(accountId)}` : ""}`}
                    data-testid="manual-trade-from-signal"
                    data-symbol={r.symbol}
                    data-side={suggestedSide(r.state)}
                    className={cn(buttonVariants({ variant: "outline", size: "sm" }))}
                  >
                    Trade
                  </Link>
                </TableCell>
              </TableRow>
            );
          })}
        </TableBody>
      </Table>
    </div>
  );
}
