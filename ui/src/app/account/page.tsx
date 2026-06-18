"use client";

import { AccountView } from "@/components/portfolio/account-view";

/**
 * Account (#5) — the PERSISTENT-book top-level, a TABBED surface. The first tab
 * is the registry CRUD ("Accounts Management"); every following tab is ONE
 * account (badged paper|live). The ACTIVE TAB selects the account — there is no
 * selector dropdown — and shows that account's positions / cash / PnL, the
 * synced EXTERNAL book, reconciliation, and Sync-from-broker (account-scoped, no
 * session needed). The loud LIVE-red treatment wraps a REAL account's tab
 * (docs/concept-alignment.md §3.4).
 */
export default function AccountPage() {
  return <AccountView />;
}
