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
import { cn } from "@/lib/utils";
import { formatNum, formatMoney } from "@/lib/format";
import {
  useStrategyIntents,
  num,
  bool,
  type StrategyIntentRow,
} from "./use-strategy-intents";
import { SortButton, csvCell, downloadCsv } from "./shared";

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

  const columns: ColumnDef<StrategyIntentRow>[] = [
    {
      key: "symbol",
      header: (
        <SortButton k="symbol" label="Symbol" sortKey={sortKey} sortDir={sortDir} onSort={onSort} />
      ),
      primary: true,
      render: (r) => {
        const buyZone = inBuyZone(num(r.intent, "proximity_to_trigger_pct"));
        return (
          <span className="flex items-center gap-1.5 font-mono font-medium">
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
        );
      },
    },
    {
      key: "proximity",
      header: (
        <SortButton k="proximity" label="%→Pivot" sortKey={sortKey} sortDir={sortDir} onSort={onSort} align="right" title="Signed distance to the pivot (buy zone: 0..2%)" />
      ),
      align: "right",
      primary: true,
      render: (r) => {
        const prox = num(r.intent, "proximity_to_trigger_pct");
        const buyZone = inBuyZone(prox);
        return (
          <span
            className={cn(
              "font-mono",
              buyZone && "font-semibold text-emerald-700 dark:text-emerald-300",
            )}
          >
            {prox != null ? `${prox > 0 ? "+" : ""}${formatNum(prox, 1)}%` : "—"}
          </span>
        );
      },
    },
    {
      key: "pivot",
      header: (
        <SortButton k="pivot" label="Pivot" sortKey={sortKey} sortDir={sortDir} onSort={onSort} align="right" />
      ),
      align: "right",
      render: (r) => {
        const pivot = num(r.intent, "pivot_price");
        return (
          <span className="font-mono">{pivot != null ? formatMoney(pivot) : "—"}</span>
        );
      },
    },
    {
      key: "stop",
      header: (
        <SortButton k="stop" label="Stop" sortKey={sortKey} sortDir={sortDir} onSort={onSort} align="right" />
      ),
      align: "right",
      render: (r) => {
        const stop = num(r.intent, "stop_price");
        return (
          <span className="font-mono text-muted-foreground">
            {stop != null ? formatMoney(stop) : "—"}
          </span>
        );
      },
    },
    {
      key: "risk",
      header: (
        <SortButton k="risk" label="Risk%" sortKey={sortKey} sortDir={sortDir} onSort={onSort} align="right" />
      ),
      align: "right",
      render: (r) => {
        const risk = num(r.intent, "risk_pct");
        return (
          <span className="font-mono">{risk != null ? `${formatNum(risk, 1)}%` : "—"}</span>
        );
      },
    },
    {
      key: "rs",
      header: (
        <SortButton k="rs" label="RS" sortKey={sortKey} sortDir={sortDir} onSort={onSort} align="right" title="Relative-strength rank (1..99)" />
      ),
      align: "right",
      render: (r) => {
        const rs = num(r.intent, "rs_rank");
        return <span className="font-mono">{rs != null ? Math.round(rs) : "—"}</span>;
      },
    },
    {
      key: "off_high",
      header: (
        <SortButton k="off_high" label="%off 52wk-hi" sortKey={sortKey} sortDir={sortDir} onSort={onSort} align="right" />
      ),
      align: "right",
      labelMobile: "%off 52wk-hi",
      render: (r) => {
        const offHigh = num(r.intent, "pct_off_52wk_high");
        return (
          <span className="font-mono">
            {offHigh != null ? `${formatNum(offHigh, 1)}%` : "—"}
          </span>
        );
      },
    },
    {
      key: "base",
      header: <span title="Base depth% / age (days)">Base</span>,
      labelMobile: "Base (depth · age)",
      render: (r) => {
        const depth = num(r.intent, "base_depth_pct");
        const age = num(r.intent, "base_age_days");
        const base =
          depth == null && age == null
            ? "—"
            : `${depth != null ? `${formatNum(depth, 1)}%` : "—"} · ${age != null ? `${Math.round(age)}d` : "—"}`;
        return <span className="font-mono text-xs text-muted-foreground">{base}</span>;
      },
    },
    {
      key: "vol",
      header: (
        <SortButton k="vol" label="Vol" sortKey={sortKey} sortDir={sortDir} onSort={onSort} align="right" title="Volume ratio vs average" />
      ),
      align: "right",
      render: (r) => {
        const vol = num(r.intent, "vol_ratio");
        return (
          <span className="font-mono">{vol != null ? `${formatNum(vol, 2)}×` : "—"}</span>
        );
      },
    },
    {
      key: "readiness",
      header: (
        <SortButton k="readiness" label="Readiness" sortKey={sortKey} sortDir={sortDir} onSort={onSort} align="right" title="Buy-readiness score (0..100)" />
      ),
      align: "right",
      primary: true,
      render: (r) => {
        // Buy-readiness, the default sort. The saturated 8/8 trend-template pass
        // is shown as a de-emphasized check, NOT 100.0.
        const readiness = num(r.intent, "buy_readiness");
        const ttPass = bool(r.intent, "trend_template_pass") === true;
        return (
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
      <ResponsiveTable
        columns={columns}
        rows={sorted}
        rowKey={(r) => r.symbol}
        rowTestId={() => "live-watchlist-row"}
        rowAttrs={(r) => {
          const readiness = num(r.intent, "buy_readiness");
          const buyZone = inBuyZone(num(r.intent, "proximity_to_trigger_pct"));
          return {
            "data-symbol": r.symbol,
            "data-buy-zone": buyZone ? "true" : "false",
            "data-readiness": readiness != null ? String(readiness) : "",
          };
        }}
        rowClassName={(r) =>
          inBuyZone(num(r.intent, "proximity_to_trigger_pct"))
            ? "bg-emerald-500/10 hover:bg-emerald-500/15 dark:bg-emerald-500/10"
            : undefined
        }
      />
    </div>
  );
}
