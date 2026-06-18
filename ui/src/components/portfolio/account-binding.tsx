import type { TradeAccountInfo } from "@/lib/api/types";

/**
 * Human label for an account: `保证金(3063)` — its label, then broker account #,
 * then venue, then id. Used by the Accounts tab strip to title each per-account
 * tab. (The old `?account=` bound-account selector + its `useBoundAccount` hook
 * were removed when the Accounts top-level became tabbed: the ACTIVE TAB now
 * selects the account, so the page no longer needs a separate binding store.)
 */
export function boundAccountLabel(a: TradeAccountInfo): string {
  return (
    a.label?.trim() ||
    (a.broker_acc_id ? String(a.broker_acc_id) : "") ||
    a.venue ||
    a.id
  );
}
