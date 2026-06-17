"use client";

import { Suspense } from "react";
import { Banknote, FlaskConical } from "lucide-react";
import { useUiMode } from "@/components/shell/ui-mode-provider";
import { cn } from "@/lib/utils";
import { useLiveSession } from "@/lib/api/hooks";
import { hasSession } from "@/lib/api/types";
import { FillsList } from "@/components/portfolio/fills-list";
import { PositionsTable } from "@/components/portfolio/positions-table";
import { Blotter } from "@/components/portfolio/blotter";
import { AccountPanel } from "@/components/portfolio/account-panel";
import type { TradeEnv } from "@/components/portfolio/trade-env";
import { TradeDeskProvider } from "./trade-desk-context";
import { OrderTicket } from "./order-ticket";
import { LiveArmSwitch } from "./live-arm-switch";
import { SyncFromBroker } from "./sync-from-broker";

/**
 * The MANUAL trading Desk for a Portfolio (Paper or Live). It places / cancels
 * orders and closes positions BY HAND against the env's bound account.
 *
 * The Desk is now ENV-bound, not mode-bound (docs/concept-alignment.md §1.3, C6):
 *   - Live (`env="live"`) — REAL money. The Desk is unconditionally LIVE-armed;
 *     the loud LIVE-REAL banner and the per-order 4-factor/confirm gating (the
 *     live phrase the OrderTicket enforces) are always on. The LiveArmSwitch is
 *     shown as the loud, guarded acknowledgement that real money is in play.
 *   - Paper (`env="paper"`) — the SIMULATE book, relaxed. No real-money arming;
 *     the order ticket confirms with the paper trade password only.
 *
 * SAFETY: env colors are deliberately distinct so an operator can never mistake
 * LIVE-REAL for PAPER. The bearer token stays server-side (every call routes
 * through the /api/proxy). The server is the authoritative gate on every
 * mutation; the UI never bypasses it.
 */
export function ManualDesk({
  env,
  accountId,
}: {
  env: TradeEnv;
  accountId?: string;
}) {
  const sessionQ = useLiveSession();
  const session = hasSession(sessionQ.data) ? sessionQ.data : null;
  const { mode } = useUiMode();
  const mobile = mode === "mobile";
  // Live env is always real-money armed (no disarm). Paper never arms live.
  const liveArmed = env === "live";

  const banner =
    env === "live"
      ? {
          label: "LIVE — REAL MONEY",
          Icon: Banknote,
          cls: "border-destructive/60 bg-destructive/10 text-destructive",
          note: "Manual orders place REAL orders against the real-money account. Confirm with the live phrase.",
        }
      : {
          label: "PAPER",
          Icon: FlaskConical,
          cls: "border-amber-500/50 bg-amber-500/10 text-amber-700 dark:text-amber-300",
          note: "Simulated fills against the SIMULATE account — no real money. Confirm with the trade password.",
        };
  const Icon = banner.Icon;

  return (
    <TradeDeskProvider>
      <main
        className={cn(
          "mx-auto w-full max-w-7xl flex-1 space-y-4",
          mobile ? "p-4" : "p-6",
        )}
        data-testid="manual-desk"
        data-env={env}
        data-live-armed={liveArmed ? "true" : "false"}
      >
        {/* Loud env safety banner (PAPER amber / LIVE-REAL destructive). */}
        <div
          className={`flex flex-wrap items-center gap-3 rounded-lg border px-4 py-2.5 ${banner.cls}`}
          data-testid="manual-mode-banner"
          data-env={env}
        >
          <Icon className="size-5 shrink-0" />
          <span className="text-sm font-semibold tracking-wide">
            {banner.label}
          </span>
          <span className="text-xs opacity-80">{banner.note}</span>
          {session?.trader_id ? (
            <span className="ml-auto font-mono text-xs opacity-70">
              {session.trader_id}
            </span>
          ) : null}
        </div>

        {/* LIVE-arm acknowledgement — shown on the Live Desk as the loud, guarded
            confirmation that real money is in play. Paper has no arming step. */}
        {env === "live" ? (
          <LiveArmSwitch armed locked onArm={() => {}} onDisarm={() => {}} />
        ) : null}

        {/* SYNC FROM BROKER (DIRECTION 2 — the operator's primary case). Pulls the
            broker's actual state into TMS so trades placed DIRECTLY in moomoo show
            up here. Read-only; safe in every env. Prominent, above the book. */}
        <SyncFromBroker />

        {/* Account (buying power + day P&L) — the env's bound account. */}
        <AccountPanel variant="desk" accountId={accountId} />

        <div
          className={cn("grid grid-cols-1 gap-4", !mobile && "lg:grid-cols-3")}
        >
          {/* Order ticket — the single client of POST /trade/order. */}
          <div className="space-y-4">
            {/* useSearchParams in the ticket needs a Suspense boundary. */}
            <Suspense fallback={null}>
              <OrderTicket liveArmed={liveArmed} />
            </Suspense>
          </div>

          {/* Book: positions (with Close) + blotter (with Cancel) + fills. */}
          <div className={cn("space-y-4", !mobile && "lg:col-span-2")}>
            <PositionsTable
              withActions
              liveArmed={liveArmed}
              accountId={accountId}
            />
            <Blotter withActions accountId={accountId} />
            <FillsList accountId={accountId} />
          </div>
        </div>
      </main>
    </TradeDeskProvider>
  );
}
