"use client";

import { cn } from "@/lib/utils";
import type { TradeAccountInfo } from "@/lib/api/types";
import { accountEnv } from "./trade-env";
import { boundAccountLabel } from "./account-binding";

/**
 * The Accounts top-level sub-navigation. The FIRST tab is always the registry
 * CRUD surface ("management"); every following tab is ONE account, generated
 * dynamically from the registry. Selecting an account tab IS the account
 * selection — there is no separate selector dropdown anymore. The active tab id
 * lives in the `?tab=` query (`management` is the default and is dropped to keep
 * the URL clean); an account tab carries that account's id.
 *
 * Each account tab is labelled by the account's `label` (falling back to env +
 * broker account #) and badged paper|live. A REAL (live) account tab gets the
 * loud destructive-red treatment so picking real money is UNMISTAKABLE.
 */

/** The management tab's id (also the default `?tab=` value). */
export const ACCOUNT_MANAGEMENT_TAB = "management";

/**
 * Resolve the active tab id from the `?tab=` query against the live account
 * list. Falls back to `management` when the value is empty/unknown (e.g. the
 * account was deleted), so a stale deep-link never strands the page.
 */
export function asAccountTab(
  value: string | null | undefined,
  accounts: TradeAccountInfo[],
): string {
  if (!value || value === ACCOUNT_MANAGEMENT_TAB) return ACCOUNT_MANAGEMENT_TAB;
  return accounts.some((a) => a.id === value)
    ? value
    : ACCOUNT_MANAGEMENT_TAB;
}

export function AccountTabs({
  accounts,
  active,
  onChange,
}: {
  accounts: TradeAccountInfo[];
  active: string;
  onChange: (tab: string) => void;
}) {
  return (
    <nav
      className="flex items-center gap-1 overflow-x-auto border-b border-border px-4 [scrollbar-width:none] ui-desktop:px-6 [&::-webkit-scrollbar]:hidden"
      data-testid="account-tabs"
    >
      <TabButton
        testid="account-tab-management"
        label="Accounts Management"
        isActive={active === ACCOUNT_MANAGEMENT_TAB}
        onClick={() => onChange(ACCOUNT_MANAGEMENT_TAB)}
      />
      {accounts.map((a) => {
        const env = accountEnv(a);
        return (
          <TabButton
            key={a.id}
            testid={`account-tab-${a.id}`}
            label={boundAccountLabel(a)}
            env={env}
            isActive={active === a.id}
            onClick={() => onChange(a.id)}
          />
        );
      })}
    </nav>
  );
}

function TabButton({
  testid,
  label,
  env,
  isActive,
  onClick,
}: {
  testid: string;
  label: string;
  env?: "paper" | "live";
  isActive: boolean;
  onClick: () => void;
}) {
  const live = env === "live";
  return (
    <button
      type="button"
      data-testid={testid}
      data-active={isActive ? "true" : "false"}
      data-env={env}
      aria-current={isActive ? "page" : undefined}
      onClick={onClick}
      className={cn(
        "flex min-h-11 shrink-0 items-center gap-1.5 whitespace-nowrap border-b-2 px-3 text-sm font-medium transition-colors ui-desktop:min-h-0 ui-desktop:py-2",
        isActive
          ? live
            ? "border-destructive text-destructive"
            : "border-primary text-foreground"
          : live
            ? "border-transparent text-destructive/70 hover:text-destructive"
            : "border-transparent text-muted-foreground hover:text-foreground",
      )}
    >
      {env ? (
        <span
          data-testid="account-tab-kind"
          data-kind={env}
          className={cn(
            "inline-flex items-center rounded-full border px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-wide",
            live
              ? "border-destructive/60 bg-destructive/10 text-destructive"
              : "border-amber-500/50 bg-amber-500/10 text-amber-700 dark:text-amber-300",
          )}
        >
          {env}
        </span>
      ) : null}
      {label}
    </button>
  );
}
