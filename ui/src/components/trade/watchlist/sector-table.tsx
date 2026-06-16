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
import { IntentStateBadge } from "@/components/trade/live-badges";
import { cn } from "@/lib/utils";
import { formatNum } from "@/lib/format";
import {
  useStrategyIntents,
  num,
  type StrategyIntentRow,
} from "./use-strategy-intents";
import { SortHead, csvCell, downloadCsv } from "./shared";

/**
 * Sector tab — the 11 sector ETFs ranked by momentum + their current rotation
 * state (hold/buy/exit/forming). A compact momentum / allocation view: rank,
 * momentum score, current rotation state, and the target-vs-current weight drift.
 *
 * Includes IDLE rows (the universe is always the full 11 ETFs even when every one
 * is `hold`/`no_setup`) — the Sector tab is a standings table, not an
 * actionable-only filter.
 */

type SectorSortKey = "rank" | "symbol" | "momentum" | "target" | "current" | "drift";

function suggestedSide(state: string): "BUY" | "SELL" {
  const s = state.toLowerCase();
  return s === "exit" || s === "sell" || s === "short" ? "SELL" : "BUY";
}

export function SectorTable({
  symbolFilter,
  accountId,
}: {
  symbolFilter: string;
  accountId?: string;
}) {
  const { rows, isLoading, error, noReader, refetch } = useStrategyIntents(
    "sector_rotation",
    { symbolFilter, includeIdle: true },
  );
  const [sortKey, setSortKey] = useState<SectorSortKey>("momentum");
  const [sortDir, setSortDir] = useState<"asc" | "desc">("desc");

  const onSort = (k: SectorSortKey) => {
    if (k === sortKey) setSortDir((d) => (d === "asc" ? "desc" : "asc"));
    else {
      setSortKey(k);
      setSortDir(k === "symbol" || k === "rank" ? "asc" : "desc");
    }
  };

  const drift = (r: StrategyIntentRow): number | null => {
    const t = num(r.intent, "target_weight");
    const c = num(r.intent, "current_weight");
    if (t == null && c == null) return null;
    return (t ?? 0) - (c ?? 0);
  };

  const sorted = useMemo(() => {
    const dir = sortDir === "asc" ? 1 : -1;
    const val = (r: StrategyIntentRow, k: SectorSortKey): number | string => {
      switch (k) {
        case "symbol":
          return r.symbol;
        case "rank": {
          const rk = num(r.intent, "rank");
          // rank 0 means "unranked" → push to the bottom on an ascending sort.
          return rk != null && rk > 0 ? rk : Infinity;
        }
        case "momentum":
          return num(r.intent, "momentum_score") ?? -Infinity;
        case "target":
          return num(r.intent, "target_weight") ?? -Infinity;
        case "current":
          return num(r.intent, "current_weight") ?? -Infinity;
        case "drift":
          return drift(r) ?? -Infinity;
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
      "rank",
      "state",
      "momentum_score",
      "target_weight",
      "current_weight",
      "drift",
    ];
    const lines = [header.join(",")];
    for (const r of sorted) {
      lines.push(
        [
          csvCell(r.symbol),
          csvCell(num(r.intent, "rank") ?? ""),
          csvCell(r.state),
          csvCell(num(r.intent, "momentum_score") ?? ""),
          csvCell(num(r.intent, "target_weight") ?? ""),
          csvCell(num(r.intent, "current_weight") ?? ""),
          csvCell(drift(r) ?? ""),
        ].join(","),
      );
    }
    downloadCsv("sector-rotation-watchlist", lines);
  };

  if (isLoading) {
    return (
      <div className="space-y-2" data-testid="sector-loading">
        <Skeleton className="h-8 w-full" />
        <Skeleton className="h-8 w-full" />
      </div>
    );
  }
  if (noReader) {
    return (
      <EmptyState
        title="Live reader not configured"
        hint="Sector standings appear once a signal session runs."
        data-testid="sector-no-reader"
      />
    );
  }
  if (error) {
    return <ErrorState error={error} onRetry={refetch} data-testid="sector-error" />;
  }
  if (sorted.length === 0) {
    return (
      <EmptyState
        title="No sector ETFs"
        hint="The 11 sector ETFs appear here as the rotation scan runs."
        data-testid="sector-empty"
      />
    );
  }

  // Weight formatter: weights arrive as fractions (0..1) — render as %.
  const pctWeight = (v: number | null) =>
    v != null ? `${formatNum(v * 100, 1)}%` : "—";

  return (
    <div className="space-y-3" data-testid="sector-table" data-row-count={sorted.length}>
      <div className="flex items-center justify-between gap-2">
        <span className="text-xs text-muted-foreground" data-testid="sector-count">
          {sorted.length} {sorted.length === 1 ? "sector ETF" : "sector ETFs"}
        </span>
        <Button
          variant="outline"
          size="sm"
          onClick={exportCsv}
          data-testid="watchlist-download"
          title="Download the sector standings as CSV"
        >
          Download CSV
        </Button>
      </div>
      <Table>
        <TableHeader>
          <TableRow>
            <SortHead k="rank" label="#" sortKey={sortKey} sortDir={sortDir} onSort={onSort} align="right" title="Momentum rank" />
            <SortHead k="symbol" label="ETF" sortKey={sortKey} sortDir={sortDir} onSort={onSort} />
            <SortHead k="momentum" label="Momentum" sortKey={sortKey} sortDir={sortDir} onSort={onSort} align="right" />
            <TableHead>State</TableHead>
            <SortHead k="target" label="Target wt" sortKey={sortKey} sortDir={sortDir} onSort={onSort} align="right" />
            <SortHead k="current" label="Current wt" sortKey={sortKey} sortDir={sortDir} onSort={onSort} align="right" />
            <SortHead k="drift" label="Drift" sortKey={sortKey} sortDir={sortDir} onSort={onSort} align="right" title="Target − current weight" />
            <TableHead className="text-right" />
          </TableRow>
        </TableHeader>
        <TableBody>
          {sorted.map((r) => {
            const rank = num(r.intent, "rank");
            const momentum = num(r.intent, "momentum_score");
            const target = num(r.intent, "target_weight");
            const current = num(r.intent, "current_weight");
            const d = drift(r);
            return (
              <TableRow
                key={r.symbol}
                data-testid="live-watchlist-row"
                data-symbol={r.symbol}
                data-rank={rank ?? ""}
              >
                <TableCell className="text-right font-mono text-muted-foreground">
                  {rank != null && rank > 0 ? rank : "—"}
                </TableCell>
                <TableCell className="font-mono font-medium">{r.symbol}</TableCell>
                <TableCell className="text-right font-mono">
                  {momentum != null ? formatNum(momentum, 2) : "—"}
                </TableCell>
                <TableCell>
                  <IntentStateBadge state={r.state} />
                </TableCell>
                <TableCell className="text-right font-mono">
                  {pctWeight(target)}
                </TableCell>
                <TableCell className="text-right font-mono text-muted-foreground">
                  {pctWeight(current)}
                </TableCell>
                <TableCell
                  className={cn(
                    "text-right font-mono",
                    d != null && d > 0 && "text-emerald-600 dark:text-emerald-400",
                    d != null && d < 0 && "text-red-600 dark:text-red-400",
                  )}
                >
                  {d != null ? `${d > 0 ? "+" : ""}${formatNum(d * 100, 1)}%` : "—"}
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
