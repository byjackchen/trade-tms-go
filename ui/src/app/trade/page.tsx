"use client";

import { Suspense } from "react";
import { PageHeader } from "@/components/shell/page-header";
import { LiveIndicator } from "@/components/trade/live-indicator";
import { TradeTabs } from "@/components/trade/trade-tabs";
import {
  AccountSelector,
  useSelectedAccount,
} from "@/components/trade/account-selector";
import { SessionBar } from "@/components/trade/session-bar";
import { ModeBanner } from "@/components/trade/mode-banner";
import { HealthStrip } from "@/components/trade/health-strip";
import { AccountPanel } from "@/components/trade/account-panel";
import { IntentsStream } from "@/components/trade/intents-stream";
import { PositionsTable } from "@/components/trade/positions-table";
import { Blotter } from "@/components/trade/blotter";
import { FillsList } from "@/components/trade/fills-list";
import { ReconciliationPanel } from "@/components/trade/reconciliation-panel";
import { SessionControls } from "@/components/trade/session-controls";

export default function TradeCockpitPage() {
  // The account selector + every account-filtered read live behind a Suspense
  // boundary because they read the `?account=` query (useSearchParams), which
  // Next requires be suspense-wrapped so prerender can fall back cleanly.
  return (
    <Suspense fallback={<CockpitBody accountId={undefined} selector={null} />}>
      <CockpitInner />
    </Suspense>
  );
}

function CockpitInner() {
  // The selected account (`?account=`) filters the positions panel, blotter and
  // account panel to one broker account; "all" leaves the books unfiltered.
  const { accountId } = useSelectedAccount();
  return <CockpitBody accountId={accountId} selector={<AccountSelector />} />;
}

function CockpitBody({
  accountId,
  selector,
}: {
  accountId: string | undefined;
  selector: React.ReactNode;
}) {
  return (
    <>
      <PageHeader
        title="Trade cockpit"
        subtitle="Portfolio overview — account, health, open positions & recent activity (read-only). Act on the desk."
        data-testid="live-header"
        actions={
          <div className="flex items-center gap-3">
            {selector}
            <LiveIndicator />
          </div>
        }
      />
      <TradeTabs />

      {/* `live-page` is the cockpit-root contract the e2e suite (specs 18-23,
          e2e/lib/live.ts liveUiReady) keys off to distinguish the real cockpit
          from the coming-soon placeholder. `live-cockpit-page` is kept for
          backward-compatible selectors. */}
      <main
        className="mx-auto w-full max-w-7xl flex-1 space-y-4 p-6"
        data-testid="live-page"
        data-cockpit="live-cockpit-page"
      >
        {/* Loud mode + halt banner (signal / PAPER / LIVE-REAL distinct colors). */}
        <ModeBanner />
        <SessionBar />

        {/* Account summary + portfolio health row (read-only overview). */}
        <div className="grid grid-cols-1 gap-4">
          <AccountPanel accountId={accountId} variant="cockpit" />
          <HealthStrip />
        </div>

        {/* Read-only book: open positions + recent orders/fills + reconciliation,
            with mode/session controls. NO order ENTRY here — acting happens on the
            desk (/trade/desk). */}
        <div className="grid grid-cols-1 gap-4 lg:grid-cols-3">
          <div className="space-y-4 lg:col-span-2">
            <PositionsTable accountId={accountId} />
            <Blotter accountId={accountId} />
            <FillsList accountId={accountId} />
            <ReconciliationPanel />
            <IntentsStream />
          </div>
          <div className="space-y-4">
            <SessionControls />
          </div>
        </div>
      </main>
    </>
  );
}
