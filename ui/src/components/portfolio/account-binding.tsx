"use client";

import { useCallback, useMemo } from "react";
import { usePathname, useRouter, useSearchParams } from "next/navigation";
import { Select } from "@/components/ui/select";
import { useAccounts } from "@/lib/api/hooks";
import { ApiError } from "@/lib/api/client";
import type { TradeAccountInfo } from "@/lib/api/types";
import { accountsForEnv, type TradeEnv } from "./trade-env";

/**
 * Resolve the account a Portfolio (`env`) is bound to. The registry
 * (GET /api/v1/trade/accounts) is filtered to the env's broker accounts
 * (paper → sim/simulate, live → real), then narrowed by the `?account=` URL query
 * if it names one of them. With no explicit query the FIRST env account is the
 * default binding — a Portfolio is always bound to a concrete account, never
 * "all".
 */
export function useBoundAccount(env: TradeEnv): {
  /** The bound account, or null while loading / when the env has no account. */
  account: TradeAccountInfo | null;
  /** The account_id to thread to the trade reads (undefined while unresolved). */
  accountId: string | undefined;
  /** All accounts available in this env (for the selector). */
  envAccounts: TradeAccountInfo[];
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

  const envAccounts = useMemo<TradeAccountInfo[]>(
    () => accountsForEnv(q.data?.accounts ?? [], env),
    [q.data, env],
  );

  const requested = search.get("account") ?? "";
  const account = useMemo<TradeAccountInfo | null>(() => {
    if (envAccounts.length === 0) return null;
    const match = envAccounts.find((a) => a.id === requested);
    return match ?? envAccounts[0]!;
  }, [envAccounts, requested]);

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
    envAccounts,
    setAccount,
    noReader,
    isLoading: q.isLoading,
  };
}

/** Human label for an env account: `保证金(3063)` (env is implied by the module). */
export function boundAccountLabel(a: TradeAccountInfo): string {
  return (
    a.label?.trim() ||
    (a.broker_acc_id ? String(a.broker_acc_id) : "") ||
    a.venue ||
    a.id
  );
}

/**
 * The Portfolio's bound-account selector. Unlike the legacy cockpit selector it
 * has NO "all accounts" entry — a Paper/Live Portfolio is always bound to one
 * concrete account in its env. Collapses to a disabled single entry when the env
 * has no account or the reader is unconfigured.
 */
export function BoundAccountSelector({ env }: { env: TradeEnv }) {
  const { account, envAccounts, setAccount, noReader } = useBoundAccount(env);
  const disabled = noReader || envAccounts.length <= 1;

  return (
    <label
      className="flex items-center gap-2 text-xs text-muted-foreground"
      data-testid="account-selector"
    >
      <span className="hidden sm:inline">Account</span>
      <Select
        aria-label="Bound account"
        data-testid="account-selector-input"
        className="h-8 w-auto min-w-[12rem]"
        value={account?.id ?? ""}
        disabled={disabled}
        onChange={(e) => setAccount(e.target.value)}
      >
        {envAccounts.length === 0 ? (
          <option value="">No {env} account</option>
        ) : (
          envAccounts.map((a) => (
            <option key={a.id} value={a.id} data-testid="account-option">
              {boundAccountLabel(a)}
            </option>
          ))
        )}
      </Select>
    </label>
  );
}
