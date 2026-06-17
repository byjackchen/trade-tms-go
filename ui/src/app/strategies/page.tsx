"use client";

import { Suspense, useState } from "react";
import { useRouter } from "next/navigation";
import { FlaskConical, Zap } from "lucide-react";
import { PageHeader } from "@/components/shell/page-header";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import { useUiMode } from "@/components/shell/ui-mode-provider";
import { useSelectedAccount } from "@/components/portfolio/account-selector";
import { StrategyLiveCard } from "@/components/strategies/strategy-live-card";
import { StrategyDetails } from "@/components/strategies/strategy-details";
import {
  StrategyWatchlist,
  type WatchlistStrategy,
} from "@/components/strategies/strategy-watchlist";
import { TunePanel } from "@/components/strategies/tune-panel";
import { NewBacktestDialog } from "@/components/compositions/new-backtest-dialog";
import type { HyperoptStrategy } from "@/lib/api/types";

/**
 * Each strategy's single-member seed Composition (docs/concept-alignment.md seeds;
 * LOCKED DECISION 5). RE-ADDING the per-strategy Backtest = backtesting this
 * single-member Composition via POST /backtests {composition_id} — there is no
 * standalone "backtest a strategy" path; a strategy backtest is always its
 * single-member Composition.
 */
const SINGLE_MEMBER_COMPOSITION: Record<string, string> = {
  sepa: "sepa-only",
  sector_rotation: "sector-only",
  pairs: "pairs-only",
  intraday_breakout: "orb-only",
};

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
  const router = useRouter();
  const { mode } = useUiMode();
  const mobile = mode === "mobile";
  const [tabId, setTabId] = useState<string>(FIRST_TAB.id);
  const tab = TABS.find((t) => t.id === tabId) ?? FIRST_TAB;
  // RE-ADDED Backtest (LOCKED DECISION 5): backtest this strategy's single-member
  // Composition. Results open in the Compositions module's inline panel via the
  // ?backtest= deep-link the NewBacktestDialog's onView routes to.
  const [backtestOpen, setBacktestOpen] = useState(false);
  const backtestCompositionId = SINGLE_MEMBER_COMPOSITION[tab.id];

  return (
    <>
      <PageHeader
        title="Strategies"
        subtitle="The four production strategies — details, watchlist, live status and tuning."
        data-testid="strategies-header"
      />

      {/* Strategy tab switcher. On mobile this becomes a horizontally-scrollable
          segmented control (no wrap) with >=44px tap targets. */}
      <nav
        className={cn(
          "flex items-center gap-1 border-b border-border px-6",
          mobile && "overflow-x-auto no-scrollbar px-4",
        )}
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
                mobile && "min-h-[44px] shrink-0 whitespace-nowrap",
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
        className={cn(
          "mx-auto w-full max-w-7xl flex-1 space-y-6",
          mobile ? "p-4" : "p-6",
        )}
        data-testid="strategies-page"
        data-active-tab={tab.id}
      >
        {/* DETAILS — every tab, ORB included. */}
        <section data-testid="strategy-section-details">
          <div className="mb-2 flex items-center justify-between gap-2">
            <h2 className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">
              Details
            </h2>
            {/* RE-ADDED per-strategy Backtest (LOCKED DECISION 5): backtests this
                strategy's single-member Composition. Hyperopt(signal) stays below. */}
            {backtestCompositionId ? (
              <Button
                size="sm"
                variant="outline"
                onClick={() => setBacktestOpen(true)}
                data-testid={`strategy-backtest-${tab.id}`}
              >
                <FlaskConical />
                Backtest
              </Button>
            ) : null}
          </div>
          <StrategyDetails strategyId={tab.id} />
        </section>

        {tab.intraday ? (
          <Alert data-testid="strategy-intraday-note">
            <Zap className="size-4" />
            <AlertTitle>Intraday strategy</AlertTitle>
            <AlertDescription>
              Intraday Breakout (ORB) trades opening-range breakouts within the
              session — it has no end-of-day watchlist and no hyperopt tuning
              here. Backtest it as a single-member Composition from the Compositions module.
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

      {/* RE-ADDED Backtest: prefilled to this strategy's single-member Composition.
          On completion we route to the Compositions module's inline backtest panel. */}
      {backtestOpen && backtestCompositionId ? (
        <NewBacktestDialog
          open
          onClose={() => setBacktestOpen(false)}
          prefillCompositionId={backtestCompositionId}
          onView={(id) => {
            setBacktestOpen(false);
            router.push(`/compositions?backtest=${id}`);
          }}
        />
      ) : null}
    </>
  );
}
