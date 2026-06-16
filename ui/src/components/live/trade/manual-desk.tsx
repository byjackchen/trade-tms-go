"use client";

import { Suspense, useState } from "react";
import { Banknote, FlaskConical, Radio } from "lucide-react";
import { useLiveSession } from "@/lib/api/hooks";
import { hasSession } from "@/lib/api/types";
import { FillsList } from "@/components/live/fills-list";
import { TradeDeskProvider } from "./trade-desk-context";
import { OrderTicket } from "./order-ticket";
import { TradePositionsPanel } from "./trade-positions-panel";
import { TradeBlotter } from "./trade-blotter";
import { ManualAccountPanel } from "./manual-account-panel";
import { LiveArmSwitch } from "./live-arm-switch";
import { SyncFromBroker } from "./sync-from-broker";

/**
 * The MANUAL trading desk root. Owns the desk-local `liveArmed` flag (the UI's
 * real-money arming state, flipped only through the guarded LiveArmSwitch) and
 * composes the desk: a loud SIGNAL / PAPER / LIVE-REAL safety banner, the order
 * ticket, the positions panel (with per-row Close), the order blotter (with
 * per-row Cancel), the fills tape, and the account snapshot.
 *
 * SAFETY: the banner colors are deliberately distinct so an operator can never
 * mistake LIVE-REAL for PAPER or SIGNAL. The bearer token stays server-side (every
 * call routes through the /api/proxy). The server is the authoritative gate on
 * every mutation; arming live in the UI never bypasses it.
 */
export function ManualDesk() {
  const sessionQ = useLiveSession();
  const session = hasSession(sessionQ.data) ? sessionQ.data : null;
  const sessionMode = String(session?.mode ?? "signal");
  // The session's strategy mode being `live` implies the desk is bound to a real
  // account; we then force the live UI on (you cannot "disarm" a genuinely live
  // session into paper from the desk). Otherwise the operator opts in via the
  // guarded switch.
  const sessionIsLive = sessionMode === "live";
  const [armed, setArmed] = useState(false);
  const liveArmed = sessionIsLive || armed;

  const banner = liveArmed
    ? {
        label: "LIVE — REAL MONEY",
        Icon: Banknote,
        cls: "border-destructive/60 bg-destructive/10 text-destructive",
        note: "Manual orders place REAL orders against the real-money account.",
      }
    : sessionMode === "paper"
      ? {
          label: "PAPER",
          Icon: FlaskConical,
          cls: "border-amber-500/50 bg-amber-500/10 text-amber-700 dark:text-amber-300",
          note: "Simulated fills against the SIMULATE account — no real money. Confirm with the trade password.",
        }
      : {
          label: "SIGNAL — manual execution",
          Icon: Radio,
          cls: "border-sky-500/40 bg-sky-500/10 text-sky-700 dark:text-sky-300",
          note: "Strategies only signal; the operator is the executor. Manual orders go to the paper/mock desk.",
        };
  const Icon = banner.Icon;

  return (
    <TradeDeskProvider>
      <main
        className="mx-auto w-full max-w-7xl flex-1 space-y-4 p-6"
        data-testid="manual-desk"
        data-mode={sessionMode}
        data-live-armed={liveArmed ? "true" : "false"}
      >
        {/* Loud mode safety banner (signal / PAPER / LIVE-REAL distinct colors). */}
        <div
          className={`flex flex-wrap items-center gap-3 rounded-lg border px-4 py-2.5 ${banner.cls}`}
          data-testid="manual-mode-banner"
          data-mode={liveArmed ? "live" : sessionMode}
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

        {/* LIVE-arm switch — the only path to real-money mode (guarded). Hidden
            when the session is already genuinely live (it is unconditionally on). */}
        {sessionIsLive ? null : (
          <LiveArmSwitch
            armed={armed}
            onArm={() => setArmed(true)}
            onDisarm={() => setArmed(false)}
          />
        )}

        {/* SYNC FROM BROKER (DIRECTION 2 — the operator's primary case). Pulls the
            broker's actual state into TMS so trades placed DIRECTLY in moomoo show
            up here. Read-only; safe in every mode. Prominent, above the book. */}
        <SyncFromBroker />

        {/* Account (buying power + day P&L). */}
        <ManualAccountPanel />

        <div className="grid grid-cols-1 gap-4 lg:grid-cols-3">
          {/* Order ticket — the single client of POST /trade/order. */}
          <div className="space-y-4">
            {/* useSearchParams in the ticket needs a Suspense boundary. */}
            <Suspense fallback={null}>
              <OrderTicket liveArmed={liveArmed} />
            </Suspense>
          </div>

          {/* Book: positions + blotter + fills. */}
          <div className="space-y-4 lg:col-span-2">
            <TradePositionsPanel liveArmed={liveArmed} />
            <TradeBlotter />
            <FillsList />
          </div>
        </div>
      </main>
    </TradeDeskProvider>
  );
}
