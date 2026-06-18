"use client";

import { Suspense, useCallback, useMemo } from "react";
import { usePathname, useRouter, useSearchParams } from "next/navigation";
import { PageHeader } from "@/components/shell/page-header";
import { cn } from "@/lib/utils";
import { useAccounts } from "@/lib/api/hooks";
import type { TradeAccountInfo } from "@/lib/api/types";
import { AccountPanel } from "./account-panel";
import { PositionsTable } from "./positions-table";
import { Blotter } from "./blotter";
import { FillsList } from "./fills-list";
import { ReconciliationPanel } from "./reconciliation-panel";
import { SyncFromBroker } from "./sync-from-broker";
import { AccountManager } from "./account-manager";
import {
  AccountTabs,
  ACCOUNT_MANAGEMENT_TAB,
  asAccountTab,
} from "./account-tabs";
import { accountEnv, type TradeEnv } from "./trade-env";

/**
 * `<AccountView />` — the ACCOUNT top-level: the PERSISTENT book, now a TABBED
 * surface. The FIRST tab is the registry CRUD ("Accounts Management"); every
 * following tab is ONE account (generated from the registry). The ACTIVE TAB
 * selects the account — there is NO selector dropdown anymore. The active tab
 * lives in `?tab=` and its account id is threaded directly into the per-account
 * book panels (AccountPanel / PositionsTable / Blotter / FillsList), while the
 * account-agnostic Reconciliation + Sync-from-broker panels render as-is.
 *
 * The "Sync from broker" control lives on each account tab (account-scoped,
 * READ-ONLY, no session needed) and works in every mode (paper/live, signal/
 * auto). Reconciliation is the panel below it.
 *
 * The loud LIVE-RED treatment (the red page ring) wraps a REAL account's tab so
 * it is UNMISTAKABLE that real money is in play.
 */
export function AccountView() {
  // `useSearchParams` (the `?tab=` reader) must sit behind a Suspense boundary —
  // Next requires it for a clean prerender fallback.
  return (
    <Suspense
      fallback={
        <AccountShell tab={ACCOUNT_MANAGEMENT_TAB} accounts={[]} onChange={() => {}} />
      }
    >
      <AccountViewInner />
    </Suspense>
  );
}

function AccountViewInner() {
  const router = useRouter();
  const pathname = usePathname();
  const search = useSearchParams();

  const q = useAccounts();
  const accounts = useMemo<TradeAccountInfo[]>(
    () => q.data?.accounts ?? [],
    [q.data],
  );
  const tab = asAccountTab(search.get("tab"), accounts);

  const onChange = useCallback(
    (next: string) => {
      const params = new URLSearchParams(search.toString());
      // `management` is the default — keep the URL clean by dropping the param.
      if (next === ACCOUNT_MANAGEMENT_TAB) params.delete("tab");
      else params.set("tab", next);
      const qs = params.toString();
      router.replace(qs ? `${pathname}?${qs}` : pathname, { scroll: false });
    },
    [router, pathname, search],
  );

  return <AccountShell tab={tab} accounts={accounts} onChange={onChange} />;
}

const MANAGEMENT_COPY = {
  title: "Accounts",
  subtitle:
    "The persistent account registry. Create, edit, delete, or set the default account per environment. Pick an account tab to open its book.",
} as const;

const ENV_COPY: Record<TradeEnv, { title: (label: string) => string; subtitle: string }> =
  {
    paper: {
      title: (label) => `Accounts — ${label}`,
      subtitle:
        "This account's persistent book: positions, cash/PnL, the synced EXTERNAL book, reconciliation, and Sync-from-broker. No session required.",
    },
    live: {
      title: (label) => `Accounts — ${label} (LIVE / REAL MONEY)`,
      subtitle:
        "A REAL-money account. This is its persistent book: positions, cash/PnL, the EXTERNAL book, and reconciliation. Switch tabs to leave live.",
    },
  };

function AccountShell({
  tab,
  accounts,
  onChange,
}: {
  tab: string;
  accounts: TradeAccountInfo[];
  onChange: (tab: string) => void;
}) {
  const account =
    tab === ACCOUNT_MANAGEMENT_TAB
      ? null
      : (accounts.find((a) => a.id === tab) ?? null);
  const env: TradeEnv = account ? accountEnv(account) : "paper";

  const title =
    account === null
      ? MANAGEMENT_COPY.title
      : ENV_COPY[env].title(accountTabLabel(account));
  const subtitle =
    account === null ? MANAGEMENT_COPY.subtitle : ENV_COPY[env].subtitle;

  return (
    // A REAL (live) account tab wraps the whole module in a loud red ring so it
    // is UNMISTAKABLE that real money is in play.
    <div
      data-env={env}
      data-testid="account-module"
      data-active-tab={tab}
      className={cn(
        env === "live" &&
          account !== null &&
          "rounded-lg border-2 border-destructive/70 shadow-[0_0_0_1px_var(--color-destructive)]",
      )}
    >
      <PageHeader title={title} subtitle={subtitle} data-testid="account-header" />
      <AccountTabs accounts={accounts} active={tab} onChange={onChange} />

      <main
        className="mx-auto w-full max-w-7xl flex-1 space-y-4 p-6 ui-mobile:p-4"
        data-testid="portfolio-view"
        data-env={env}
      >
        {account === null ? (
          /* MANAGE ACCOUNTS — the registry CRUD surface (create / edit / delete /
             set-default). The account tabs read the same registry, so changes
             here refresh them immediately. */
          <AccountManager />
        ) : (
          <AccountBook account={account} />
        )}
      </main>
    </div>
  );
}

/** Tab label for an account: its label, falling back to `env broker#`. */
function accountTabLabel(a: TradeAccountInfo): string {
  const label = a.label?.trim();
  if (label) return label;
  const env = a.env?.trim() || accountEnv(a);
  return a.broker_acc_id ? `${env} ${a.broker_acc_id}` : env;
}

/**
 * One account's whole ledger, scoped to its id. The account-filtered panels
 * (AccountPanel / PositionsTable / Blotter / FillsList) take the id as a prop;
 * Reconciliation + Sync-from-broker are account-agnostic reads and render as-is.
 */
function AccountBook({ account }: { account: TradeAccountInfo }) {
  const env = accountEnv(account);
  return (
    <div className="space-y-4" data-testid="account-book" data-env={env}>
      {/* SYNC FROM BROKER (DIRECTION 2). Account-scoped, READ-ONLY, no session
          needed — pulls externally-placed positions into the EXTERNAL book. */}
      <SyncFromBroker />

      {/* ACCOUNT — funds / buying-power / day-pnl for this account. */}
      <AccountPanel accountId={account.id} variant="portfolio" />

      {/* POSITIONS — this account's read-only book (strategy + EXTERNAL). */}
      <div className="space-y-4">
        <PositionsTable accountId={account.id} />
        <Blotter accountId={account.id} />
        <FillsList accountId={account.id} />
        <ReconciliationPanel />
      </div>
    </div>
  );
}
