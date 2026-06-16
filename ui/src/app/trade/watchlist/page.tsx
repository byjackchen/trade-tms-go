"use client";

import { Suspense } from "react";
import { PageHeader } from "@/components/shell/page-header";
import { LiveIndicator } from "@/components/trade/live-indicator";
import { TradeTabs } from "@/components/trade/trade-tabs";
import { SessionBar } from "@/components/trade/session-bar";
import { useSelectedAccount } from "@/components/trade/account-selector";
import { WatchlistTabs } from "@/components/trade/watchlist/watchlist-tabs";

/**
 * The per-strategy watchlist (`/trade/watchlist`). A SEPA | Sector | Pairs tab
 * switcher over shared filters (account selector + symbol search). Each tab is a
 * purpose-built table reading the watchlist intents filtered by strategy_id.
 */
export default function TradeWatchlistPage() {
  return (
    <Suspense fallback={<WatchlistBody accountId={undefined} />}>
      <WatchlistInner />
    </Suspense>
  );
}

function WatchlistInner() {
  const { accountId } = useSelectedAccount();
  return <WatchlistBody accountId={accountId} />;
}

function WatchlistBody({ accountId }: { accountId: string | undefined }) {
  return (
    <>
      <PageHeader
        title="Watchlist"
        subtitle="Per-strategy trade plans — SEPA breakouts, sector rotation, and pairs."
        data-testid="live-watchlist-header"
        actions={<LiveIndicator />}
      />
      <TradeTabs />

      <main
        className="mx-auto w-full max-w-6xl flex-1 space-y-4 p-6"
        data-testid="live-watchlist-page"
      >
        <SessionBar />
        <WatchlistTabs accountId={accountId} />
      </main>
    </>
  );
}
