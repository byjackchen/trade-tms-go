"use client";

import { useState } from "react";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { AccountSelector } from "@/components/portfolio/account-selector";
import { cn } from "@/lib/utils";
import { SepaTable } from "./sepa-table";
import { SectorTable } from "./sector-table";
import { PairsTable } from "./pairs-table";

type TabId = "sepa" | "sector" | "pairs";

const TABS: { id: TabId; label: string; testid: string }[] = [
  { id: "sepa", label: "SEPA", testid: "watchlist-tab-sepa" },
  { id: "sector", label: "Sector", testid: "watchlist-tab-sector" },
  { id: "pairs", label: "Pairs", testid: "watchlist-tab-pairs" },
];

/**
 * The per-strategy watchlist. A SEPA | Sector | Pairs tab switcher over shared
 * filters (the account selector + a symbol search). Each tab is its own
 * purpose-built table reading the watchlist intents filtered by `strategy_id`:
 *
 *   - SEPA   — the actionable trade-plan (pivot/stop/risk/RS/readiness), buy-zone
 *              highlighted, Trade → desk prefill.
 *   - Sector — the 11 sector ETFs ranked by momentum + rotation state.
 *   - Pairs  — one row per pair (z-score vs thresholds, hedge ratio, half-life).
 *
 * Keeps the `live-watchlist` root + `watchlist-search` / `watchlist-download` /
 * `live-watchlist-row` testid contract the e2e suite (specs 20, 34) depends on.
 */
export function WatchlistTabs({ accountId }: { accountId?: string } = {}) {
  const [tab, setTab] = useState<TabId>("sepa");
  const [query, setQuery] = useState("");

  return (
    <Card data-testid="live-watchlist" data-panel="watchlist-tabs" data-active-tab={tab}>
      <CardHeader className="gap-3">
        <div className="flex flex-wrap items-center justify-between gap-2">
          <CardTitle className="text-sm">Watchlist</CardTitle>
        </div>

        {/* Shared filters above the tabs: account selector + symbol search. */}
        <div className="flex flex-wrap items-center gap-2" data-testid="watchlist-filters">
          <AccountSelector />
          <Input
            type="search"
            placeholder="Search symbol…"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            className="h-8 w-40"
            data-testid="watchlist-search"
            aria-label="Search symbol"
          />
        </div>

        {/* Strategy tab switcher. */}
        <nav
          className="flex items-center gap-1 border-b border-border"
          data-testid="watchlist-tabs"
          role="tablist"
        >
          {TABS.map((t) => {
            const active = tab === t.id;
            return (
              <button
                key={t.id}
                type="button"
                role="tab"
                aria-selected={active}
                data-testid={t.testid}
                data-active={active ? "true" : "false"}
                onClick={() => setTab(t.id)}
                className={cn(
                  "border-b-2 px-3 py-2 text-sm font-medium transition-colors",
                  active
                    ? "border-primary text-foreground"
                    : "border-transparent text-muted-foreground hover:text-foreground",
                )}
              >
                {t.label}
              </button>
            );
          })}
        </nav>
      </CardHeader>
      <CardContent>
        {tab === "sepa" ? (
          <SepaTable symbolFilter={query} accountId={accountId} />
        ) : tab === "sector" ? (
          <SectorTable symbolFilter={query} accountId={accountId} />
        ) : (
          <PairsTable symbolFilter={query} accountId={accountId} />
        )}
      </CardContent>
    </Card>
  );
}
