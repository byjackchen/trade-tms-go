"use client";

import { Suspense } from "react";
import { PageHeader } from "@/components/shell/page-header";
import { cn } from "@/lib/utils";
import { LiveIndicator } from "./live-indicator";
import { ExecBanner } from "./exec-banner";
import { HealthStrip } from "./health-strip";
import { AccountPanel } from "./account-panel";
import { PositionsTable } from "./positions-table";
import { Blotter } from "./blotter";
import { FillsList } from "./fills-list";
import { ReconciliationPanel } from "./reconciliation-panel";
import { SyncFromBroker } from "./sync-from-broker";
import { BoundAccountSelector, useBoundAccount } from "./account-binding";
import type { TradeEnv } from "./trade-env";

/**
 * `<AccountView />` — the ACCOUNT top-level: the PERSISTENT book. It owns account
 * SELECTION (the BoundAccountSelector lists ALL accounts, each badged paper|live)
 * and renders the selected account's whole ledger: account summary + portfolio
 * health, positions, blotter, fills, reconciliation, and the synced EXTERNAL book.
 *
 * The "Sync from broker" control lives HERE (account-scoped, READ-ONLY, no session
 * needed) and works in every mode (paper/live, signal/auto). Reconciliation is the
 * panel below it.
 *
 * The loud LIVE-RED treatment (the red page ring + the destructive ExecBanner)
 * lives here because account SELECTION lives here — picking a REAL account must be
 * UNMISTAKABLE.
 */
export function AccountView() {
  // Bound-account resolution + every account-filtered read live behind a Suspense
  // boundary because they read `?account=` (useSearchParams), which Next requires
  // be suspense-wrapped. The fallback assumes paper (no account resolved yet).
  return (
    <Suspense fallback={<AccountHeader env="paper" selector={null} />}>
      <AccountViewInner />
    </Suspense>
  );
}

function AccountViewInner() {
  const { accountId, env } = useBoundAccount();

  return (
    // When a REAL (live) account is selected the whole module gets a loud red ring
    // so it is UNMISTAKABLE that real money is in play.
    <div
      data-env={env}
      data-testid="account-module"
      className={cn(
        env === "live" &&
          "rounded-lg border-2 border-destructive/70 shadow-[0_0_0_1px_var(--color-destructive)]",
      )}
    >
      <AccountHeader env={env} selector={<BoundAccountSelector />} />
      <main
        className={cn(
          "mx-auto w-full max-w-7xl flex-1 space-y-4 p-6 ui-mobile:p-4",
        )}
        data-testid="portfolio-view"
        data-env={env}
      >
        {/* Loud exec + env banner (PAPER amber / LIVE-REAL destructive). */}
        <ExecBanner env={env} />

        {/* SYNC FROM BROKER (DIRECTION 2). Account-scoped, READ-ONLY, no session
            needed — pulls externally-placed positions into the EXTERNAL book.
            Prominent, above the book. */}
        <SyncFromBroker />

        {/* ACCOUNT — the bound account's summary + portfolio health. */}
        <div className="grid grid-cols-1 gap-4">
          <AccountPanel accountId={accountId} variant="portfolio" />
          <HealthStrip />
        </div>

        {/* POSITIONS — the account's read-only book (strategy + EXTERNAL). */}
        <div className="space-y-4">
          <PositionsTable accountId={accountId} />
          <Blotter accountId={accountId} />
          <FillsList accountId={accountId} />
          <ReconciliationPanel />
        </div>
      </main>
    </div>
  );
}

const ENV_COPY: Record<TradeEnv, { title: string; subtitle: string }> = {
  paper: {
    title: "Accounts",
    subtitle:
      "The persistent book — pick an account above. Positions, cash/PnL, the synced EXTERNAL book, reconciliation, and Sync-from-broker. No session required.",
  },
  live: {
    title: "Accounts — LIVE (REAL MONEY)",
    subtitle:
      "A REAL-money account is selected. This is its persistent book: positions, cash/PnL, the EXTERNAL book, and reconciliation. Switch the account above to leave live.",
  },
};

function AccountHeader({
  env,
  selector,
}: {
  env: TradeEnv;
  selector: React.ReactNode;
}) {
  const copy = ENV_COPY[env];
  return (
    <PageHeader
      title={copy.title}
      subtitle={copy.subtitle}
      data-testid="account-header"
      actions={
        <div className="flex items-center gap-3">
          {selector}
          <LiveIndicator />
        </div>
      }
    />
  );
}
