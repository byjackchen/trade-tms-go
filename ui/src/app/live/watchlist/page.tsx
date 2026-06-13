import { PageHeader } from "@/components/shell/page-header";
import { LiveIndicator } from "@/components/live/live-indicator";
import { LiveTabs } from "@/components/live/live-tabs";
import { SessionBar } from "@/components/live/session-bar";
import { WatchlistTable } from "@/components/live/watchlist-table";

export default function LiveWatchlistPage() {
  return (
    <>
      <PageHeader
        title="Watchlist"
        subtitle="The tracked universe with each symbol's latest live intent."
        data-testid="live-watchlist-header"
        actions={<LiveIndicator />}
      />
      <LiveTabs />

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
