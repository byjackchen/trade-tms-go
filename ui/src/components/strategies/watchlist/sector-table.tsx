"use client";

import { useMemo, useState } from "react";
import Link from "next/link";
import {
  ResponsiveTable,
  type ColumnDef,
} from "@/components/ui/responsive-table";
import { Skeleton } from "@/components/ui/skeleton";
import { Button, buttonVariants } from "@/components/ui/button";
import { EmptyState, ErrorState } from "@/components/shell/states";
import { IntentStateBadge } from "@/components/portfolio/live-badges";
import { cn } from "@/lib/utils";
import { formatNum } from "@/lib/format";
import {
  useStrategyIntents,
  num,
  type StrategyIntentRow,
} from "./use-strategy-intents";
import { SortButton, csvCell, downloadCsv } from "./shared";

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

  const columns: ColumnDef<StrategyIntentRow>[] = [
    {
      key: "rank",
      header: (
        <SortButton k="rank" label="#" sortKey={sortKey} sortDir={sortDir} onSort={onSort} align="right" title="Momentum rank" />
      ),
      align: "right",
      labelMobile: "Rank",
      render: (r) => {
        const rank = num(r.intent, "rank");
        return (
          <span className="font-mono text-muted-foreground">
            {rank != null && rank > 0 ? rank : "—"}
          </span>
        );
      },
    },
    {
      key: "symbol",
      header: (
        <SortButton k="symbol" label="ETF" sortKey={sortKey} sortDir={sortDir} onSort={onSort} />
      ),
      primary: true,
      render: (r) => <span className="font-mono font-medium">{r.symbol}</span>,
    },
    {
      key: "momentum",
      header: (
        <SortButton k="momentum" label="Momentum" sortKey={sortKey} sortDir={sortDir} onSort={onSort} align="right" />
      ),
      align: "right",
      primary: true,
      render: (r) => {
        const momentum = num(r.intent, "momentum_score");
        return (
          <span className="font-mono">{momentum != null ? formatNum(momentum, 2) : "—"}</span>
        );
      },
    },
    {
      key: "state",
      header: "State",
      primary: true,
      render: (r) => <IntentStateBadge state={r.state} />,
    },
    {
      key: "target",
      header: (
        <SortButton k="target" label="Target wt" sortKey={sortKey} sortDir={sortDir} onSort={onSort} align="right" />
      ),
      align: "right",
      render: (r) => (
        <span className="font-mono">{pctWeight(num(r.intent, "target_weight"))}</span>
      ),
    },
    {
      key: "current",
      header: (
        <SortButton k="current" label="Current wt" sortKey={sortKey} sortDir={sortDir} onSort={onSort} align="right" />
      ),
      align: "right",
      render: (r) => (
        <span className="font-mono text-muted-foreground">
          {pctWeight(num(r.intent, "current_weight"))}
        </span>
      ),
    },
    {
      key: "drift",
      header: (
        <SortButton k="drift" label="Drift" sortKey={sortKey} sortDir={sortDir} onSort={onSort} align="right" title="Target − current weight" />
      ),
      align: "right",
      render: (r) => {
        const d = drift(r);
        return (
          <span
            className={cn(
              "font-mono",
              d != null && d > 0 && "text-emerald-600 dark:text-emerald-400",
              d != null && d < 0 && "text-red-600 dark:text-red-400",
            )}
          >
            {d != null ? `${d > 0 ? "+" : ""}${formatNum(d * 100, 1)}%` : "—"}
          </span>
        );
      },
    },
    {
      key: "trade",
      header: <span className="sr-only">Trade</span>,
      labelMobile: "Action",
      align: "right",
      primary: true,
      render: (r) => (
        <Link
          href={`/trade?view=desk&symbol=${encodeURIComponent(r.symbol)}&side=${suggestedSide(r.state)}${accountId ? `&account=${encodeURIComponent(accountId)}` : ""}`}
          data-testid="manual-trade-from-signal"
          data-symbol={r.symbol}
          data-side={suggestedSide(r.state)}
          className={cn(buttonVariants({ variant: "outline", size: "sm" }))}
        >
          Trade
        </Link>
      ),
    },
  ];

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
      <ResponsiveTable
        columns={columns}
        rows={sorted}
        rowKey={(r) => r.symbol}
        rowTestId={() => "live-watchlist-row"}
        rowAttrs={(r) => {
          const rank = num(r.intent, "rank");
          return {
            "data-symbol": r.symbol,
            "data-rank": rank != null ? String(rank) : "",
          };
        }}
      />
    </div>
  );
}
