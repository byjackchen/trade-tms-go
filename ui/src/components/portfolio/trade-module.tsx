"use client";

import { Suspense } from "react";
import { usePathname, useSearchParams } from "next/navigation";
import Link from "next/link";
import { PageHeader } from "@/components/shell/page-header";
import { cn } from "@/lib/utils";
import { LiveIndicator } from "./live-indicator";
import { ExecBanner } from "./exec-banner";
import { SessionBar } from "./session-bar";
import { HealthStrip } from "./health-strip";
import { AccountPanel } from "./account-panel";
import { PositionsTable } from "./positions-table";
import { Blotter } from "./blotter";
import { FillsList } from "./fills-list";
import { ReconciliationPanel } from "./reconciliation-panel";
import { SessionControls } from "./session-controls";
import { ManualDesk } from "./desk/manual-desk";
import { BoundAccountSelector, useBoundAccount } from "./account-binding";
import type { TradeEnv } from "./trade-env";

type View = "portfolio" | "desk";

const ENV_COPY: Record<
  TradeEnv,
  { title: string; subtitle: string }
> = {
  paper: {
    title: "Paper Trade",
    subtitle:
      "The SIMULATE book — Portfolio (positions, account, blotter, fills, reconciliation, health) and the manual Desk. No real money.",
  },
  live: {
    title: "Live Trade",
    subtitle:
      "The REAL-money book — Portfolio (positions, account, blotter, fills, reconciliation, health) and the manual Desk. Live-armed, server-gated.",
  },
};

/**
 * `<TradeModule env="paper"|"live" />` — the SINGLE shared trading surface, mounted
 * twice against different accounts (Paper → sim/simulate book, Live → real book).
 * Both modules are byte-identical apart from the bound env and its gating
 * (docs/concept-alignment.md §3.4): Live keeps the loud LIVE-REAL banner + the
 * per-order confirm gating; Paper is relaxed.
 *
 * It resolves the env's bound account (useAccounts filtered by env) and renders
 * two views, switched by `?view=`:
 *   - Portfolio — the runtime book: account panel + positions + blotter + fills +
 *     reconciliation + health (all read-only, account-filtered).
 *   - Desk — the order ticket / close / sync-from-broker / live-arm.
 */
export function TradeModule({ env }: { env: TradeEnv }) {
  // The bound-account resolution and every account-filtered read live behind a
  // Suspense boundary because they read the `?account=` / `?view=` query
  // (useSearchParams), which Next requires be suspense-wrapped so prerender can
  // fall back cleanly.
  return (
    <Suspense fallback={<ModuleHeader env={env} selector={null} view="portfolio" />}>
      <TradeModuleInner env={env} />
    </Suspense>
  );
}

function TradeModuleInner({ env }: { env: TradeEnv }) {
  const search = useSearchParams();
  const view: View = search.get("view") === "desk" ? "desk" : "portfolio";
  const { accountId } = useBoundAccount(env);

  return (
    <>
      <ModuleHeader env={env} selector={<BoundAccountSelector env={env} />} view={view} />
      {view === "desk" ? (
        <ManualDesk env={env} accountId={accountId} />
      ) : (
        <PortfolioView env={env} accountId={accountId} />
      )}
    </>
  );
}

function ModuleHeader({
  env,
  selector,
  view,
}: {
  env: TradeEnv;
  selector: React.ReactNode;
  view: View;
}) {
  const copy = ENV_COPY[env];
  return (
    <>
      <PageHeader
        title={copy.title}
        subtitle={copy.subtitle}
        data-testid={env === "live" ? "live-header" : "paper-header"}
        actions={
          <div className="flex items-center gap-3">
            {selector}
            <LiveIndicator />
          </div>
        }
      />
      <ModuleTabs view={view} />
    </>
  );
}

/** Portfolio / Desk sub-nav, preserving the `?account=` binding across views. */
function ModuleTabs({ view }: { view: View }) {
  const pathname = usePathname();
  const search = useSearchParams();
  const account = search.get("account");
  const acctQs = account ? `&account=${encodeURIComponent(account)}` : "";
  const tabs: { view: View; label: string; testid: string }[] = [
    { view: "portfolio", label: "Portfolio", testid: "tab-portfolio" },
    { view: "desk", label: "Desk", testid: "tab-desk" },
  ];
  return (
    <nav
      className="flex items-center gap-1 border-b border-border px-6"
      data-testid="trade-module-tabs"
    >
      {tabs.map((t) => {
        const active = view === t.view;
        const href =
          t.view === "portfolio"
            ? `${pathname}${account ? `?account=${encodeURIComponent(account)}` : ""}`
            : `${pathname}?view=desk${acctQs}`;
        return (
          <Link
            key={t.view}
            href={href}
            data-testid={t.testid}
            data-active={active ? "true" : "false"}
            aria-current={active ? "page" : undefined}
            className={cn(
              "border-b-2 px-3 py-2 text-sm font-medium transition-colors",
              active
                ? "border-primary text-foreground"
                : "border-transparent text-muted-foreground hover:text-foreground",
            )}
          >
            {t.label}
          </Link>
        );
      })}
    </nav>
  );
}

/**
 * The Portfolio (runtime book) view: the env account's ledger — account panel +
 * portfolio-health row, then the read-only open positions / blotter / fills /
 * reconciliation. NO order ENTRY here — acting happens on the Desk tab.
 */
function PortfolioView({
  env,
  accountId,
}: {
  env: TradeEnv;
  accountId: string | undefined;
}) {
  return (
    <main
      className="mx-auto w-full max-w-7xl flex-1 space-y-4 p-6"
      data-testid="portfolio-view"
      data-env={env}
    >
      {/* Loud exec + env banner (PAPER amber / LIVE-REAL destructive). */}
      <ExecBanner env={env} />
      <SessionBar />

      {/* Account summary + portfolio health row (read-only overview). */}
      <div className="grid grid-cols-1 gap-4">
        <AccountPanel accountId={accountId} variant="portfolio" />
        <HealthStrip />
      </div>

      {/* Read-only book: positions + recent orders/fills + reconciliation, with
          session controls. */}
      <div className="grid grid-cols-1 gap-4 lg:grid-cols-3">
        <div className="space-y-4 lg:col-span-2">
          <PositionsTable accountId={accountId} />
          <Blotter accountId={accountId} />
          <FillsList accountId={accountId} />
          <ReconciliationPanel />
        </div>
        <div className="space-y-4">
          <SessionControls env={env} />
        </div>
      </div>
    </main>
  );
}
