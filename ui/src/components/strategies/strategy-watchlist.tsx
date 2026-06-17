"use client";

import { useState } from "react";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { AccountSelector } from "@/components/portfolio/account-selector";
import { SepaTable } from "./watchlist/sepa-table";
import { SectorTable } from "./watchlist/sector-table";
import { PairsTable } from "./watchlist/pairs-table";

/** The watchlist-bearing strategies. ORB has no watchlist (it is intraday). */
export type WatchlistStrategy = "sepa" | "sector" | "pairs";

/**
 * One strategy's WATCHLIST, rendered on its Strategies tab. The per-strategy
 * trade-plan table (SEPA breakouts / sector rotation / pairs z-score) over a
 * shared account selector + symbol search. Relocated from the old
 * `/trade/watchlist` tab switcher (docs/concept-alignment.md §3.4 ②): each tab
 * now owns exactly one table, so there is no inner tab strip.
 *
 * Keeps the `live-watchlist` root + `watchlist-search` testid contract the e2e
 * suite depends on.
 */
export function StrategyWatchlist({
  strategy,
  accountId,
}: {
  strategy: WatchlistStrategy;
  accountId?: string;
}) {
  const [query, setQuery] = useState("");

  return (
    <Card
      data-testid="live-watchlist"
      data-panel="watchlist"
      data-strategy={strategy}
    >
      <CardHeader className="gap-3">
        <div className="flex flex-wrap items-center justify-between gap-2">
          <CardTitle className="text-sm">Watchlist</CardTitle>
        </div>
        <div
          className="flex flex-wrap items-center gap-2"
          data-testid="watchlist-filters"
        >
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
      </CardHeader>
      <CardContent>
        {strategy === "sepa" ? (
          <SepaTable symbolFilter={query} accountId={accountId} />
        ) : strategy === "sector" ? (
          <SectorTable symbolFilter={query} accountId={accountId} />
        ) : (
          <PairsTable symbolFilter={query} accountId={accountId} />
        )}
      </CardContent>
    </Card>
  );
}
