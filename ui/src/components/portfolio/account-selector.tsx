"use client";

import { useCallback, useMemo } from "react";
import { usePathname, useRouter, useSearchParams } from "next/navigation";
import { Select } from "@/components/ui/select";
import { useAccounts } from "@/lib/api/hooks";
import { ApiError } from "@/lib/api/client";
import type { TradeAccountInfo } from "@/lib/api/types";

/** The sentinel selector value meaning "all accounts" (no `account_id` filter). */
export const ALL_ACCOUNTS = "all";

/**
 * Read the currently-selected account from the `?account=` URL query and a setter
 * that writes it back (shareable + sticky across the cockpit/desk tabs).
 *
 * The returned `accountId` is the value to pass to the trade reads as
 * `account_id`: `undefined` when "all" is selected (so the hooks omit the param
 * and the unattributed rows stay visible), or a concrete registry id otherwise.
 */
export function useSelectedAccount(): {
  account: string;
  accountId: string | undefined;
  setAccount: (id: string) => void;
} {
  const router = useRouter();
  const pathname = usePathname();
  const search = useSearchParams();
  const account = search.get("account") ?? ALL_ACCOUNTS;

  const setAccount = useCallback(
    (id: string) => {
      const params = new URLSearchParams(search.toString());
      if (!id || id === ALL_ACCOUNTS) params.delete("account");
      else params.set("account", id);
      const qs = params.toString();
      router.replace(qs ? `${pathname}?${qs}` : pathname, { scroll: false });
    },
    [router, pathname, search],
  );

  return {
    account,
    accountId: account === ALL_ACCOUNTS ? undefined : account,
    setAccount,
  };
}

/** Human label for an account, env-prefixed: `real · 保证金(3063)`. */
export function accountLabel(a: TradeAccountInfo): string {
  // Prefer the registry label; fall back to the broker account number; finally
  // the venue. Always env-prefixed so paper/live/sim are unmistakable.
  const tail =
    a.label?.trim() ||
    (a.broker_acc_id ? String(a.broker_acc_id) : "") ||
    a.venue ||
    a.id;
  const head = a.env || a.venue;
  return head ? `${head} · ${tail}` : tail;
}

/**
 * The cockpit/desk account selector. Renders the `tms.accounts` registry as an
 * env-labeled dropdown; selecting an account writes `?account=` to the URL, which
 * the positions panel / blotter / account panel read back as their `account_id`
 * filter. "All accounts" clears the filter (and the param).
 *
 * Degrades quietly: when the trade reader is unconfigured (503) or empty, the
 * control disables to a single "All accounts" entry rather than erroring.
 */
export function AccountSelector() {
  const { account, setAccount } = useSelectedAccount();
  const q = useAccounts();
  const accounts = useMemo<TradeAccountInfo[]>(
    () => q.data?.accounts ?? [],
    [q.data],
  );
  const noReader = q.error instanceof ApiError && q.error.status === 503;
  const disabled = noReader || accounts.length === 0;

  return (
    <label
      className="flex items-center gap-2 text-xs text-muted-foreground"
      data-testid="account-selector"
    >
      <span className="hidden sm:inline">Account</span>
      <Select
        aria-label="Trading account"
        data-testid="account-selector-input"
        className="h-8 w-auto min-w-[12rem]"
        value={account}
        disabled={disabled}
        onChange={(e) => setAccount(e.target.value)}
      >
        <option value={ALL_ACCOUNTS}>All accounts</option>
        {accounts.map((a) => (
          <option key={a.id} value={a.id} data-testid="account-option">
            {accountLabel(a)}
          </option>
        ))}
        {/* A shared/bookmarked ?account=<id> may point at an account still
            loading, errored, or no longer in the registry. The reads are already
            filtered by the URL id, so surface a synthetic option for it — the
            visible selection then matches the active filter (rather than silently
            reading "All accounts" while the book is filtered) and the operator can
            switch back to All. */}
        {account !== ALL_ACCOUNTS && !accounts.some((a) => a.id === account) ? (
          <option value={account} data-testid="account-option-unknown">
            {account}
          </option>
        ) : null}
      </Select>
    </label>
  );
}
