"use client";

import { useMemo, useState } from "react";
import {
  ResponsiveTable,
  type ColumnDef,
} from "@/components/ui/responsive-table";
import { Skeleton } from "@/components/ui/skeleton";
import { Button } from "@/components/ui/button";
import { EmptyState, ErrorState } from "@/components/shell/states";
import { IntentStateBadge } from "@/components/portfolio/live-badges";
import { cn } from "@/lib/utils";
import { formatNum } from "@/lib/format";
import { useStrategySignals, num, str } from "./use-strategy-intents";
import { SortButton, csvCell, downloadCsv } from "./shared";

/**
 * Pairs tab — ONE row per pair (pair_id), not per leg. The pairs strategy emits a
 * leg intent per symbol (long + short); we collapse the two legs into a single
 * pair row keyed by `pair_id`, taking the freshest leg's spread metrics (they are
 * pair-level: z-score, hedge ratio, half-life, thresholds are identical on both
 * legs) and showing which symbol is the long leg vs the short leg.
 *
 * Columns: Pair | z-score (vs entry/exit thresholds) | Hedge ratio | Half-life |
 * State | Long leg | Short leg. The |z| / entry-threshold ratio shows how
 * stretched each pair is (a stretch bar). Sorted DESC by |z| by default.
 */

type PairsSortKey = "pair" | "z" | "hedge" | "halflife" | "stretch";

type PairRow = {
  pair_id: string;
  state: string;
  z_score: number | null;
  hedge_ratio: number | null;
  half_life_days: number | null;
  z_entry: number | null;
  z_exit: number | null;
  long_leg: string | null;
  short_leg: string | null;
  tsMs: number;
};

export function PairsTable({ symbolFilter }: { symbolFilter: string; accountId?: string }) {
  // Include idle: a pair can sit at no_setup while still being a tracked pair we
  // want to show how close it is to its entry threshold.
  const { rows, isLoading, error, noReader, refetch } = useStrategySignals(
    "pairs",
    { symbolFilter, includeIdle: true },
  );
  const [sortKey, setSortKey] = useState<PairsSortKey>("stretch");
  const [sortDir, setSortDir] = useState<"asc" | "desc">("desc");

  const onSort = (k: PairsSortKey) => {
    if (k === sortKey) setSortDir((d) => (d === "asc" ? "desc" : "asc"));
    else {
      setSortKey(k);
      setSortDir(k === "pair" ? "asc" : "desc");
    }
  };

  // Collapse leg intents into one row per pair_id.
  const pairs = useMemo(() => {
    const byPair = new Map<string, PairRow>();
    for (const r of rows) {
      const i = r.intent;
      const pairId = str(i, "pair_id") ?? r.symbol;
      const legRole = (str(i, "leg_role") ?? "").toLowerCase();
      const existing = byPair.get(pairId);
      const row: PairRow = existing ?? {
        pair_id: pairId,
        state: r.state,
        z_score: num(i, "z_score"),
        hedge_ratio: num(i, "hedge_ratio"),
        half_life_days: num(i, "half_life_days"),
        z_entry: num(i, "z_entry_threshold"),
        z_exit: num(i, "z_exit_threshold"),
        long_leg: null,
        short_leg: null,
        tsMs: r.tsMs,
      };
      // Freshest leg's pair-level metrics win.
      if (r.tsMs >= row.tsMs) {
        row.tsMs = r.tsMs;
        row.state = r.state;
        row.z_score = num(i, "z_score") ?? row.z_score;
        row.hedge_ratio = num(i, "hedge_ratio") ?? row.hedge_ratio;
        row.half_life_days = num(i, "half_life_days") ?? row.half_life_days;
        row.z_entry = num(i, "z_entry_threshold") ?? row.z_entry;
        row.z_exit = num(i, "z_exit_threshold") ?? row.z_exit;
      }
      if (legRole === "long") row.long_leg = r.symbol;
      else if (legRole === "short") row.short_leg = r.symbol;
      byPair.set(pairId, row);
    }
    return [...byPair.values()];
  }, [rows]);

  // Stretch = |z| / entry threshold (≥1 means past entry).
  const stretchOf = (p: PairRow): number | null => {
    if (p.z_score == null || p.z_entry == null || p.z_entry === 0) return null;
    return Math.abs(p.z_score) / Math.abs(p.z_entry);
  };

  const sorted = useMemo(() => {
    const dir = sortDir === "asc" ? 1 : -1;
    const val = (p: PairRow, k: PairsSortKey): number | string => {
      switch (k) {
        case "pair":
          return p.pair_id;
        case "z":
          return p.z_score != null ? Math.abs(p.z_score) : -Infinity;
        case "hedge":
          return p.hedge_ratio ?? -Infinity;
        case "halflife":
          return p.half_life_days ?? -Infinity;
        case "stretch":
          return stretchOf(p) ?? -Infinity;
      }
    };
    return [...pairs].sort((a, b) => {
      const va = val(a, sortKey);
      const vb = val(b, sortKey);
      let r: number;
      if (typeof va === "string" || typeof vb === "string")
        r = String(va).localeCompare(String(vb));
      else r = va - vb;
      return r !== 0 ? r * dir : a.pair_id.localeCompare(b.pair_id);
    });
  }, [pairs, sortKey, sortDir]);

  const exportCsv = () => {
    const header = [
      "pair_id",
      "state",
      "z_score",
      "z_entry_threshold",
      "z_exit_threshold",
      "hedge_ratio",
      "half_life_days",
      "long_leg",
      "short_leg",
    ];
    const lines = [header.join(",")];
    for (const p of sorted) {
      lines.push(
        [
          csvCell(p.pair_id),
          csvCell(p.state),
          csvCell(p.z_score ?? ""),
          csvCell(p.z_entry ?? ""),
          csvCell(p.z_exit ?? ""),
          csvCell(p.hedge_ratio ?? ""),
          csvCell(p.half_life_days ?? ""),
          csvCell(p.long_leg ?? ""),
          csvCell(p.short_leg ?? ""),
        ].join(","),
      );
    }
    downloadCsv("pairs-watchlist", lines);
  };

  if (isLoading) {
    return (
      <div className="space-y-2" data-testid="pairs-loading">
        <Skeleton className="h-8 w-full" />
        <Skeleton className="h-8 w-full" />
      </div>
    );
  }
  if (noReader) {
    return (
      <EmptyState
        title="Live reader not configured"
        hint="Tracked pairs appear once a signal session runs."
        data-testid="pairs-no-reader"
      />
    );
  }
  if (error) {
    return <ErrorState error={error} onRetry={refetch} data-testid="pairs-error" />;
  }
  if (sorted.length === 0) {
    return (
      <EmptyState
        title="No tracked pairs"
        hint="Cointegrated pairs appear here as the scan runs."
        data-testid="pairs-empty"
      />
    );
  }

  const columns: ColumnDef<PairRow>[] = [
    {
      key: "pair",
      header: (
        <SortButton k="pair" label="Pair" sortKey={sortKey} sortDir={sortDir} onSort={onSort} />
      ),
      primary: true,
      render: (p) => <span className="font-mono font-medium">{p.pair_id}</span>,
    },
    {
      key: "z",
      header: (
        <SortButton k="z" label="z-score" sortKey={sortKey} sortDir={sortDir} onSort={onSort} align="right" title="Spread z-score vs entry/exit thresholds" />
      ),
      align: "right",
      primary: true,
      render: (p) => (
        <span className="font-mono">
          {p.z_score != null ? formatNum(p.z_score, 2) : "—"}
          {p.z_entry != null ? (
            <span className="ml-1 text-[10px] text-muted-foreground">
              /±{formatNum(p.z_entry, 1)}
            </span>
          ) : null}
        </span>
      ),
    },
    {
      key: "stretch",
      header: (
        <SortButton k="stretch" label="Stretch" sortKey={sortKey} sortDir={sortDir} onSort={onSort} align="right" title="|z| / entry threshold (≥1 = past entry)" />
      ),
      align: "right",
      primary: true,
      render: (p) => {
        const stretch = stretchOf(p);
        const stretched = stretch != null && stretch >= 1;
        return (
          <span
            className={cn(
              "font-mono",
              stretched && "font-semibold text-amber-700 dark:text-amber-300",
            )}
          >
            {stretch != null ? `${formatNum(stretch * 100, 0)}%` : "—"}
          </span>
        );
      },
    },
    {
      key: "hedge",
      header: (
        <SortButton k="hedge" label="Hedge" sortKey={sortKey} sortDir={sortDir} onSort={onSort} align="right" title="Hedge ratio (β)" />
      ),
      align: "right",
      render: (p) => (
        <span className="font-mono">
          {p.hedge_ratio != null ? formatNum(p.hedge_ratio, 3) : "—"}
        </span>
      ),
    },
    {
      key: "halflife",
      header: (
        <SortButton k="halflife" label="Half-life" sortKey={sortKey} sortDir={sortDir} onSort={onSort} align="right" title="Mean-reversion half-life (days)" />
      ),
      align: "right",
      render: (p) => (
        <span className="font-mono">
          {p.half_life_days != null ? `${formatNum(p.half_life_days, 1)}d` : "—"}
        </span>
      ),
    },
    {
      key: "state",
      header: "State",
      primary: true,
      render: (p) => <IntentStateBadge state={p.state} />,
    },
    {
      key: "legs",
      header: "Long / Short",
      render: (p) => (
        <span className="font-mono text-xs">
          <span className="text-emerald-600 dark:text-emerald-400">
            {p.long_leg ?? "—"}
          </span>
          <span className="mx-1 text-muted-foreground">/</span>
          <span className="text-red-600 dark:text-red-400">
            {p.short_leg ?? "—"}
          </span>
        </span>
      ),
    },
  ];

  return (
    <div className="space-y-3" data-testid="pairs-table" data-row-count={sorted.length}>
      <div className="flex items-center justify-between gap-2">
        <span className="text-xs text-muted-foreground" data-testid="pairs-count">
          {sorted.length} {sorted.length === 1 ? "pair" : "pairs"}
        </span>
        <Button
          variant="outline"
          size="sm"
          onClick={exportCsv}
          data-testid="watchlist-download"
          title="Download the tracked pairs as CSV"
        >
          Download CSV
        </Button>
      </div>
      <ResponsiveTable
        columns={columns}
        rows={sorted}
        rowKey={(p) => p.pair_id}
        rowTestId={() => "live-watchlist-row"}
        rowAttrs={(p) => {
          const stretch = stretchOf(p);
          const stretched = stretch != null && stretch >= 1;
          return {
            "data-symbol": p.pair_id,
            "data-pair-id": p.pair_id,
            "data-stretched": stretched ? "true" : "false",
          };
        }}
        rowClassName={(p) => {
          const stretch = stretchOf(p);
          return stretch != null && stretch >= 1 ? "bg-amber-500/10" : undefined;
        }}
      />
    </div>
  );
}
