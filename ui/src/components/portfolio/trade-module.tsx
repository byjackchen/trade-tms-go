"use client";

import { Suspense } from "react";
import { usePathname, useSearchParams } from "next/navigation";
import Link from "next/link";
import { PageHeader } from "@/components/shell/page-header";
import { useUiMode } from "@/components/shell/ui-mode-provider";
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

/**
 * `<TradeModule />` — the SINGLE unified trading surface that replaced the old
 * Paper + Live pages. There is NO fixed `env` prop anymore: the bound account
 * comes ONLY from the top-right account selector (which lists ALL accounts, each
 * badged paper|live). The SELECTED account's env (derived from its server `kind`)
 * drives everything the old per-page split did (docs/concept-alignment.md §3.4):
 *   - the loud LIVE-RED treatment (banner + page border) when a REAL account is
 *     selected — UNMISTAKABLE since the page no longer separates paper vs live;
 *   - SessionControls' SIGNAL/AUTO arm-confirm gating;
 *   - the 4-factor / confirm gate (these already read the account, not the page).
 *
 * Two views, switched by `?view=`:
 *   - Portfolio — the runtime book: account panel + positions + blotter + fills +
 *     reconciliation + health (read-only, account-filtered).
 *   - Desk — the order ticket / close / sync-from-broker / live-arm.
 */
export function TradeModule() {
  // The bound-account resolution and every account-filtered read live behind a
  // Suspense boundary because they read the `?account=` / `?view=` query
  // (useSearchParams), which Next requires be suspense-wrapped so prerender can
  // fall back cleanly. The fallback assumes paper (no account resolved yet).
  return (
    <Suspense fallback={<ModuleHeader env="paper" selector={null} view="portfolio" />}>
      <TradeModuleInner />
    </Suspense>
  );
}

function TradeModuleInner() {
  const search = useSearchParams();
  const view: View = search.get("view") === "desk" ? "desk" : "portfolio";
  const { accountId, env } = useBoundAccount();

  return (
    // When a REAL (live) account is selected the whole module gets a loud red
    // ring so it is UNMISTAKABLE — the page no longer separates paper vs live.
    <div
      data-env={env}
      data-testid="trade-module"
      className={cn(
        env === "live" &&
          "rounded-lg border-2 border-destructive/70 shadow-[0_0_0_1px_var(--color-destructive)]",
      )}
    >
      <ModuleHeader env={env} selector={<BoundAccountSelector />} view={view} />
      {view === "desk" ? (
        <ManualDesk env={env} accountId={accountId} />
      ) : (
        <PortfolioView env={env} accountId={accountId} />
      )}
    </div>
  );
}

const ENV_COPY: Record<TradeEnv, { title: string; subtitle: string }> = {
  paper: {
    title: "Trade",
    subtitle:
      "Unified trading surface — pick an account above. PAPER (sim/simulate): Portfolio (positions, account, blotter, fills, reconciliation, health) and the manual Desk. No real money.",
  },
  live: {
    title: "Trade — LIVE (REAL MONEY)",
    subtitle:
      "Unified trading surface — a REAL-money account is selected. Live-armed, server-gated. Every order is REAL. Switch the account above to leave live.",
  },
};

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
        data-testid="trade-header"
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

/** Portfolio / Desk sub-nav, preserving the `?account=` binding across views.
 * On MOBILE it becomes a horizontally-scrollable segmented control with >=44px
 * tap targets; on DESKTOP the underlined tab bar is unchanged. */
function ModuleTabs({ view }: { view: View }) {
  const pathname = usePathname();
  const search = useSearchParams();
  const { mode } = useUiMode();
  const mobile = mode === "mobile";
  const account = search.get("account");
  const acctQs = account ? `&account=${encodeURIComponent(account)}` : "";
  const tabs: { view: View; label: string; testid: string }[] = [
    { view: "portfolio", label: "Portfolio", testid: "tab-portfolio" },
    { view: "desk", label: "Desk", testid: "tab-desk" },
  ];
  return (
    <nav
      className={cn(
        "flex items-center border-b border-border",
        mobile
          ? // Mobile: horizontally-scrollable segmented control, edge-to-edge.
            "gap-2 overflow-x-auto px-4 py-2 [scrollbar-width:none] [&::-webkit-scrollbar]:hidden"
          : "gap-1 px-6",
      )}
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
              "text-sm font-medium transition-colors",
              mobile
                ? // Segmented pill, >=44px tall, never shrinks below tap size.
                  cn(
                    "flex min-h-11 shrink-0 items-center rounded-full px-4",
                    active
                      ? "bg-primary text-primary-foreground"
                      : "bg-muted text-muted-foreground hover:text-foreground",
                  )
                : cn(
                    "border-b-2 px-3 py-2",
                    active
                      ? "border-primary text-foreground"
                      : "border-transparent text-muted-foreground hover:text-foreground",
                  ),
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
 * The Portfolio (runtime book) view: the selected account's ledger — account
 * panel + portfolio-health row, then the read-only open positions / blotter /
 * fills / reconciliation. NO order ENTRY here — acting happens on the Desk tab.
 */
function PortfolioView({
  env,
  accountId,
}: {
  env: TradeEnv;
  accountId: string | undefined;
}) {
  const { mode } = useUiMode();
  const mobile = mode === "mobile";
  return (
    <main
      className={cn(
        "mx-auto w-full max-w-7xl flex-1 space-y-4",
        // Tighter padding on mobile so panels get the full container width.
        mobile ? "p-4" : "p-6",
      )}
      data-testid="portfolio-view"
      data-env={env}
    >
      {/* Loud exec + env banner (PAPER amber / LIVE-REAL destructive) — env now
          comes from the selected account. */}
      <ExecBanner env={env} />
      <SessionBar />

      {/* Account summary + portfolio health row (read-only overview). */}
      <div className="grid grid-cols-1 gap-4">
        <AccountPanel accountId={accountId} variant="portfolio" />
        <HealthStrip />
      </div>

      {/* Read-only book: positions + recent orders/fills + reconciliation, with
          session controls. On mobile this stacks into a single column (the
          explicit mobile cookie collapses the desktop 3-col split regardless of
          viewport width — LOCKED DECISION 4). */}
      <div className={cn("grid grid-cols-1 gap-4", !mobile && "lg:grid-cols-3")}>
        <div className={cn("space-y-4", !mobile && "lg:col-span-2")}>
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
