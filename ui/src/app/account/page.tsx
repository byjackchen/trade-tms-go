"use client";

import { AccountView } from "@/components/portfolio/account-view";

/**
 * Account (#5) — the PERSISTENT-book top-level. Account selection (the
 * BoundAccountSelector lists ALL accounts, each badged paper|live), the selected
 * account's positions / cash / PnL, the synced EXTERNAL book, reconciliation, and
 * Sync-from-broker (account-scoped, no session needed). The loud LIVE-red
 * treatment lives here, where account selection happens
 * (docs/concept-alignment.md §3.4).
 */
export default function AccountPage() {
  return <AccountView />;
}
