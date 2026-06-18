"use client";

import { useCallback, useMemo } from "react";
import { usePathname, useRouter, useSearchParams } from "next/navigation";
import { Select } from "@/components/ui/select";
import { useAccounts } from "@/lib/api/hooks";
import { ApiError } from "@/lib/api/client";
import type { TradeAccountInfo } from "@/lib/api/types";
import { accountEnv, type TradeEnv } from "./trade-env";

/**
 * Resolve the account the unified `/trade` module is bound to. The registry
 * (GET /api/v1/trade/accounts) lists ALL accounts — there is NO env filter
 * anymore (Paper + Live are one module). The `?account=` URL query selects one;
 * with no explicit query the FIRST account is the default binding (a Portfolio is
 * always bound to a concrete account, never "all"). The SELECTED account's env
 * (derived from its `kind`) is what drives the LIVE-red treatment and gating.
 */
export function useBoundAccount(): {
  /** The bound account, or null while loading / when there are no accounts. */
  account: TradeAccountInfo | null;
  /** The account_id to thread to the trade reads (undefined while unresolved). */
  accountId: string | undefined;
  /** The selected account's env ("paper"|"live"), derived from its kind. */
  env: TradeEnv;
  /** All registered accounts (for the selector). */
  accounts: TradeAccountInfo[];
  /** Switch the bound account (writes `?account=`). */
  setAccount: (id: string) => void;
  /** True when the trade reader is unconfigured (503). */
  noReader: boolean;
  isLoading: boolean;
} {
  const router = useRouter();
  const pathname = usePathname();
  const search = useSearchParams();
  const q = useAccounts();

  const accounts = useMemo<TradeAccountInfo[]>(
    () => q.data?.accounts ?? [],
    [q.data],
  );

  const requested = search.get("account") ?? "";
  const account = useMemo<TradeAccountInfo | null>(() => {
    if (accounts.length === 0) return null;
    const match = accounts.find((a) => a.id === requested);
    return match ?? accounts[0]!;
  }, [accounts, requested]);

  const setAccount = useCallback(
    (id: string) => {
      const params = new URLSearchParams(search.toString());
      if (!id) params.delete("account");
      else params.set("account", id);
      const qs = params.toString();
      router.replace(qs ? `${pathname}?${qs}` : pathname, { scroll: false });
    },
    [router, pathname, search],
  );

  const noReader = q.error instanceof ApiError && q.error.status === 503;

  return {
    account,
    accountId: account?.id,
    // The bound env is the selected account's env; default to paper while unresolved.
    env: account ? accountEnv(account) : "paper",
    accounts,
    setAccount,
    noReader,
    isLoading: q.isLoading,
  };
}

/** Human label for an account: `保证金(3063)` (the paper|live badge is shown beside it). */
export function boundAccountLabel(a: TradeAccountInfo): string {
  return (
    a.label?.trim() ||
    (a.broker_acc_id ? String(a.broker_acc_id) : "") ||
    a.venue ||
    a.id
  );
}

/** A small paper|live pill for one option/selected account, colored by env. */
function KindBadge({ env }: { env: TradeEnv }) {
  return (
    <span
      data-testid="account-kind-badge"
      data-kind={env}
      className={
        env === "live"
          ? "inline-flex items-center rounded-full border border-destructive/60 bg-destructive/10 px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-wide text-destructive"
          : "inline-flex items-center rounded-full border border-amber-500/50 bg-amber-500/10 px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-wide text-amber-700 dark:text-amber-300"
      }
    >
      {env}
    </span>
  );
}

/**
 * The unified /trade bound-account selector. It lists ALL accounts (no env
 * filter) and badges each one paper|live from its server-derived `kind`, so the
 * operator picks any account and the page reconfigures around it. Selecting a
 * REAL (live) account makes the whole page loud-red (see AccountView). Collapses
 * to a disabled single entry when there are no accounts / the reader is
 * unconfigured.
 */
export function BoundAccountSelector() {
  const { account, accounts, setAccount, noReader } = useBoundAccount();
  const disabled = noReader || accounts.length <= 1;
  const selectedEnv = account ? accountEnv(account) : "paper";

  return (
    <div
      className="flex items-center gap-2 text-xs text-muted-foreground"
      data-testid="account-selector"
    >
      <label className="flex items-center gap-2">
        <span className="hidden sm:inline">Account</span>
        <Select
          aria-label="Bound account"
          data-testid="account-selector-input"
          className="h-8 w-auto min-w-[12rem]"
          value={account?.id ?? ""}
          disabled={disabled}
          onChange={(e) => setAccount(e.target.value)}
        >
          {accounts.length === 0 ? (
            <option value="">No accounts</option>
          ) : (
            accounts.map((a) => (
              <option key={a.id} value={a.id} data-testid="account-option">
                {accountEnv(a) === "live" ? "● LIVE" : "○ paper"}{" "}
                {boundAccountLabel(a)}
              </option>
            ))
          )}
        </Select>
      </label>
      {/* The selected account's paper|live badge (a <select> can't render rich
          option markup, so the live pill lives beside the control). */}
      {account ? <KindBadge env={selectedEnv} /> : null}
    </div>
  );
}
