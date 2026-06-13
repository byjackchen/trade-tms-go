import { PageHeader } from "@/components/shell/page-header";
import { LiveIndicator } from "@/components/live/live-indicator";
import { LiveTabs } from "@/components/live/live-tabs";
import { SessionBar } from "@/components/live/session-bar";
import { HealthStrip } from "@/components/live/health-strip";
import { IntentsStream } from "@/components/live/intents-stream";
import { SessionControls } from "@/components/live/session-controls";
import { WatchlistTable } from "@/components/live/watchlist-table";

export default function LiveCockpitPage() {
  return (
    <>
      <PageHeader
        title="Live cockpit"
        subtitle="Signal intents, portfolio health and session control — live."
        data-testid="live-header"
        actions={<LiveIndicator />}
      />
      <LiveTabs />

      {/* `live-page` is the cockpit-root contract the e2e suite (specs 18-23,
          e2e/lib/live.ts liveUiReady) keys off to distinguish the real cockpit
          from the coming-soon placeholder. `live-cockpit-page` is kept for
          backward-compatible selectors. */}
      <main
        className="mx-auto w-full max-w-7xl flex-1 space-y-4 p-6"
        data-testid="live-page"
        data-cockpit="live-cockpit-page"
      >
        <SessionBar />

        <div className="grid grid-cols-1 gap-4 lg:grid-cols-3">
          <div className="space-y-4 lg:col-span-2">
            <HealthStrip />
            <IntentsStream />
          </div>
          <div className="space-y-4">
            <SessionControls />
            <WatchlistTable />
          </div>
        </div>
      </main>
    </>
  );
}
