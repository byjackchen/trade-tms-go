"use client";

import { Suspense, useState } from "react";
import { Zap } from "lucide-react";
import { PageHeader } from "@/components/shell/page-header";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { cn } from "@/lib/utils";
import { useSelectedAccount } from "@/components/portfolio/account-selector";
import { StrategyLiveCard } from "@/components/strategies/strategy-live-card";
import { StrategyDetails } from "@/components/strategies/strategy-details";
import {
  StrategyWatchlist,
  type WatchlistStrategy,
} from "@/components/strategies/strategy-watchlist";
import { TunePanel } from "@/components/strategies/tune-panel";
import type { HyperoptStrategy } from "@/lib/api/types";

/**
 * One per-strategy tab.
 *
 * `id` is the canonical strategy id (GET /strategies/{id}, the strategy_state
 * stream). `watchlist` / `tune` are present only for the EOD strategies — ORB is
 * intraday, so it shows DETAILS only (no watchlist, no hyperopt;
 * docs/concept-alignment.md §3.4 A4).
 */
type StrategyTab = {
  id: string;
  label: string;
  testid: string;
  watchlist?: WatchlistStrategy;
  tune?: HyperoptStrategy;
  intraday?: boolean;
};

const TABS: StrategyTab[] = [
  { id: "sepa", label: "SEPA", testid: "strategy-tab-sepa", watchlist: "sepa", tune: "sepa" },
  {
    id: "sector_rotation",
    label: "Sector Rotation",
    testid: "strategy-tab-sector",
    watchlist: "sector",
    tune: "sector_rotation",
  },
  { id: "pairs", label: "Pairs", testid: "strategy-tab-pairs", watchlist: "pairs", tune: "pairs" },
  {
    id: "intraday_breakout",
    label: "Intraday Breakout (ORB)",
    testid: "strategy-tab-orb",
    intraday: true,
  },
];

// TABS is statically non-empty; `!` documents that to the type checker so the
// fallback below is a real value, not `T | undefined`.
const FIRST_TAB: StrategyTab = TABS[0]!;

/**
 * The Strategies module (docs/concept-alignment.md §3.4 ②, the TUNE stage). A tab
 * per production strategy; each tab shows that strategy's DETAILS (resolved
 * params + schema/source), its WATCHLIST, live status, and the Tune (hyperopt)
 * panel. ORB is intraday: details only.
 */
export default function StrategiesPage() {
  return (
    <Suspense fallback={null}>
      <StrategiesInner />
    </Suspense>
  );
}

function StrategiesInner() {
  const { accountId } = useSelectedAccount();
  return <StrategiesBody accountId={accountId} />;
}

function StrategiesBody({ accountId }: { accountId: string | undefined }) {
  const [tabId, setTabId] = useState<string>(FIRST_TAB.id);
  const tab = TABS.find((t) => t.id === tabId) ?? FIRST_TAB;

  return (
    <>
      <PageHeader
        title="Strategies"
        subtitle="The four production strategies — details, watchlist, live status and tuning."
        data-testid="strategies-header"
      />

      {/* Strategy tab switcher. */}
      <nav
        className="flex items-center gap-1 border-b border-border px-6"
        data-testid="strategy-tabs"
        role="tablist"
      >
        {TABS.map((t) => {
          const active = t.id === tabId;
          return (
            <button
              key={t.id}
              type="button"
              role="tab"
              aria-selected={active}
              data-testid={t.testid}
              data-active={active ? "true" : "false"}
              onClick={() => setTabId(t.id)}
              className={cn(
                "border-b-2 px-3 py-2.5 text-sm font-medium transition-colors",
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

      <main
        className="mx-auto w-full max-w-7xl flex-1 space-y-6 p-6"
        data-testid="strategies-page"
        data-active-tab={tab.id}
      >
        {/* DETAILS — every tab, ORB included. */}
        <section data-testid="strategy-section-details">
          <h2 className="mb-2 text-xs font-semibold uppercase tracking-wide text-muted-foreground">
            Details
          </h2>
          <StrategyDetails strategyId={tab.id} />
        </section>

        {tab.intraday ? (
          <Alert data-testid="strategy-intraday-note">
            <Zap className="size-4" />
            <AlertTitle>Intraday strategy</AlertTitle>
            <AlertDescription>
              Intraday Breakout (ORB) trades opening-range breakouts within the
              session — it has no end-of-day watchlist and no hyperopt tuning
              here. Backtest it as a single-member Model from the Models module.
            </AlertDescription>
          </Alert>
        ) : (
          <>
            {/* Live status. */}
            <section data-testid="strategy-section-live">
              <h2 className="mb-2 text-xs font-semibold uppercase tracking-wide text-muted-foreground">
                Live status
              </h2>
              <StrategyLiveCard strategyId={tab.id} label={tab.label} />
            </section>

            {/* Watchlist. */}
            {tab.watchlist ? (
              <section data-testid="strategy-section-watchlist">
                <h2 className="mb-2 text-xs font-semibold uppercase tracking-wide text-muted-foreground">
                  Watchlist
                </h2>
                <StrategyWatchlist
                  strategy={tab.watchlist}
                  accountId={accountId}
                />
              </section>
            ) : null}

            {/* Tune (hyperopt). */}
            {tab.tune ? (
              <section data-testid="strategy-section-tune">
                <TunePanel strategy={tab.tune} />
              </section>
            ) : null}
          </>
        )}
      </main>
    </>
  );
}
