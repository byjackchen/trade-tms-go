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
 * book panels (AccountPanel / PositionsTable / Blotter / FillsList).
 *
 * Broker-sync + reconciliation (DIRECTION 2) act on the live trade node's bound
 * account, NOT an arbitrary registry account, so they render ONCE on the Accounts
 * Management tab — not duplicated (misleadingly) under every account tab.
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
    "The persistent account registry. Create, edit, delete, or set the default account per environment; sync + reconcile the live trade node below. Pick an account tab to open its book.",
} as const;

const ENV_COPY: Record<TradeEnv, { title: (label: string) => string; subtitle: string }> =
  {
    paper: {
      title: (label) => `Accounts — ${label}`,
      subtitle:
        "This account's persistent book: positions, cash/PnL, and the synced EXTERNAL book. No session required.",
    },
    live: {
      title: (label) => `Accounts — ${label} (LIVE / REAL MONEY)`,
      subtitle:
        "A REAL-money account. This is its persistent book: positions, cash/PnL, and the EXTERNAL book. Switch tabs to leave live.",
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
             set-default), plus the NODE-LEVEL broker-sync + reconciliation (they
             act on the live trade node's bound account — DIRECTION 2 — not a
             specific registry account, so they belong here, not per-account-tab). */
          <div className="space-y-4">
            <AccountManager />
            <SyncFromBroker />
            <ReconciliationPanel />
          </div>
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
 * One account's book, scoped to its id via the account-filtered panels
 * (AccountPanel / PositionsTable / Blotter / FillsList all take the id as a prop).
 * Broker-sync + reconciliation are NOT here: they act on the live trade node's
 * bound account (not an arbitrary registry account), so they live once on the
 * Accounts Management tab rather than misleadingly under every account tab.
 */
function AccountBook({ account }: { account: TradeAccountInfo }) {
  const env = accountEnv(account);
  return (
    <div className="space-y-4" data-testid="account-book" data-env={env}>
      {/* ACCOUNT — funds / buying-power / day-pnl for this account. */}
      <AccountPanel accountId={account.id} variant="portfolio" />

      {/* POSITIONS — this account's read-only book (strategy + EXTERNAL). */}
      <div className="space-y-4">
        <PositionsTable accountId={account.id} />
        <Blotter accountId={account.id} />
        <FillsList accountId={account.id} />
      </div>
    </div>
  );
}
