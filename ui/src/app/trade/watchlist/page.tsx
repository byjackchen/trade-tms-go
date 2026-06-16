import { PageHeader } from "@/components/shell/page-header";
import { LiveIndicator } from "@/components/trade/live-indicator";
import { TradeTabs } from "@/components/trade/trade-tabs";
import { SessionBar } from "@/components/trade/session-bar";
import { WatchlistTable } from "@/components/trade/watchlist-table";

export default function TradeWatchlistPage() {
  return (
    <>
      <PageHeader
        title="Watchlist"
        subtitle="The tracked universe with each symbol's latest live intent."
        data-testid="live-watchlist-header"
        actions={<LiveIndicator />}
      />
      <TradeTabs />

      <main
        className="mx-auto w-full max-w-5xl flex-1 space-y-4 p-6"
        data-testid="live-watchlist-page"
      >
        <SessionBar />
        <WatchlistTable />
      </main>
    </>
  );
}
