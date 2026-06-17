/**
 * Trade cockpit e2e helpers (P5).
 *
 * Post-restructure the cockpit is the trade module, mounted at /paper (sim book)
 * and /live (real book) — the old /trade route now 301-redirects to /paper. It
 * is the read surface over the trade node: signal intents streaming in over the
 * WS (bridged from Redis, durable truth in tms.signal_intents), portfolio health
 * + session state, the watchlist (tracked universe), and the audited control
 * surface (halt / kill-switch / exec-policy toggle via POST
 * /api/v1/trade/commands). See docs/api.md "Trade (P5)" and
 * docs/spec/{ui-runner-modes-eod,portfolio-risk}.md.
 *
 * GATE TOPOLOGY (P5 decisions 2/3/7): the gate runs a signal session
 * (exec_policy=signal) against the in-repo MOCK OpenD server, which replays a day
 * of bars out of our Postgres — so a signal session emits intents into
 * tms.signal_intents + the Redis streams the API bridges to WS, with NO real
 * OpenD. The real-OpenD smoke is deferred to market hours
 * (docs/runbooks/trade-smoke.md).
 *
 * BUILD ORDER: the trade cockpit + the trade read endpoints land in P5, after
 * the P1 Data / P2 Backtests / P3 Strategies+Hyperopt workspaces. These specs
 * are PERMANENT and assert the documented contract; while the section is still
 * the coming-soon placeholder, the trade reader is absent (endpoints 503), or no
 * signal session has run yet, they self-skip cleanly so the gate stays green —
 * exactly like specs 07-17 did for their workspaces before they landed.
 *
 * Ground truth is read independently from postgres (lib/db) and the Go API
 * (lib/api) — the UI renders the UI's proxy of the API, and both must agree
 * with the DB. No fabricated values.
 */

import type { Page } from "@playwright/test";
import { getAuthed, getManual } from "./api";
import { withDb, latestSession, type LiveSessionTruth } from "./db";

/** Canonical strategy-id discriminators that appear on streaming intents
 * (lowercase SignalIntentUnion discriminator — api spec §5.9, migration 000005
 * CHECK on tms.signal_intents.strategy_id). */
export const INTENT_STRATEGY_IDS = [
  "sepa",
  "pairs",
  "sector_rotation",
  "intraday_breakout",
] as const;

/** Valid signal states (migration 000005 CHECK). */
export const INTENT_STATES = [
  "no_setup",
  "forming",
  "buy",
  "hold",
  "exit",
  "stop_hit",
] as const;

/**
 * True once the trade module (the post-restructure cockpit) is rendered.
 *
 * Post-restructure (docs/concept-alignment.md): the old `/trade` cockpit was
 * replaced by the two <TradeModule> mounts at `/paper` (sim book) and `/live`
 * (real book). The default "is the cockpit ready" target is `/paper` — it has a
 * bound paper account, whereas `/live` has no real account registered and is
 * empty. The module's ready signal is the `paper-header` testid (rendered by
 * both the Portfolio and Desk views of the paper module — ui/src/components/
 * portfolio/trade-module.tsx). Returns false when the app-shell or the module
 * header never appears (route not built / not implemented).
 */
export async function liveUiReady(page: Page): Promise<boolean> {
  await page.goto("/paper", { waitUntil: "domcontentloaded" });
  const shell = page.getByTestId("app-shell");
  try {
    await shell.waitFor({ state: "visible", timeout: 15_000 });
  } catch {
    return false;
  }
  const header = page.getByTestId("paper-header");
  try {
    await header.waitFor({ state: "visible", timeout: 15_000 });
    return true;
  } catch {
    return false;
  }
}

/**
 * Whether the API was started WITH a live reader. The live read endpoints
 * return 503 when the API has no live reader configured (docs/api.md "Live
 * (P5)": "The live read endpoints return 503 when the API was started without a
 * live reader."). A 200 OR a `{session:null}` 200 means the reader is present;
 * a 503 means the live surface is unavailable and the cockpit specs must skip.
 *
 * We probe GET /api/v1/trade/session: 503 → no reader; anything else (200 with a
 * session or {session:null}, even 404-ish) → reader present.
 */
export async function liveReaderAvailable(): Promise<boolean> {
  const res = await getAuthed("trade/session");
  return res.status !== 503;
}

/** The most recent session, or null. Used to gate session-dependent assertions
 * (a signal session must have run for intents/health/watchlist to be populated). */
export async function currentSession(): Promise<LiveSessionTruth | null> {
  return withDb((c) => latestSession(c));
}

/** A RUNNING signal session exists (the gate's `tms trade run --mode signal` node).
 * Several specs require a live emitter to be running; they skip otherwise. */
export async function hasRunningSignalSession(): Promise<boolean> {
  const s = await currentSession();
  return !!s && s.mode === "signal" && s.status === "RUNNING";
}

// ---------------------------------------------------------------------------
// Paper/live TRADING helpers (P6). The gate runs `tms trade run --mode paper`
// against the in-repo MOCK trading venue (an extension of the P5 mock OpenD):
// it accepts Trd_PlaceOrder, simulates accept->fill (or reject), pushes
// Trd_UpdateOrder / Trd_UpdateOrderFill, and maintains mock positions/funds
// (P6 decision 9). The MoomooExecutor (decision 2) maps domain orders onto the
// venue and the order-state machine (decision 3) onto tms.orders/fills, with the
// portfolio gate (decision 4) as a PRE-SUBMIT check that writes tms.risk_events.
//
// These helpers gate the paper-trading specs the same way the signal specs gate
// on a signal session: they self-skip cleanly until the paper session + the
// cockpit's paper-trading panels land, so the gate stays green meanwhile (the
// established specs-07-17 pattern). The contract they bind is documented in
// docs/api.md "Live trading (P6, paper/live)" and docs/spec/ui-runner-modes-eod.md.
// ---------------------------------------------------------------------------

/**
 * Whether the API was started WITH a trading reader (the LiveStore implementing
 * LiveTradingReader, internal/api/trade_trading.go). The trading read endpoints
 * (/trade/orders, /trade/positions, /trade/account, /trade/reconciliation) return
 * 503 "unavailable" when no trading reader is configured. We probe
 * GET /api/v1/trade/positions: 503 → no trading reader; any other status (200
 * with a possibly-empty book) → reader present.
 *
 * This is strictly stronger than liveReaderAvailable() (which only needs the
 * signal reader): a stack can have the signal reader but no trading reader.
 */
export async function liveTradingAvailable(): Promise<boolean> {
  const res = await getAuthed("trade/positions");
  return res.status !== 503;
}

/** A RUNNING paper session exists (the gate's `tms trade run --mode paper` node over
 * the mock venue). The paper-trading specs require it; they skip otherwise. */
export async function hasRunningPaperSession(): Promise<boolean> {
  const s = await currentSession();
  return !!s && s.mode === "paper" && s.status === "RUNNING";
}

/** A RUNNING paper OR live trading session exists. Trading specs that only need
 * a position book (not specifically paper) gate on this. The gate only ever
 * runs paper (live is never auto-activated — decision 8); this is the broader
 * predicate so a future live-canary stack would also satisfy it. */
export async function hasRunningTradingSession(): Promise<boolean> {
  const s = await currentSession();
  return (
    !!s &&
    (s.mode === "paper" || s.mode === "live") &&
    s.status === "RUNNING"
  );
}

/**
 * Wait until a UI panel identified by any of `testids` becomes visible, polling
 * up to `timeout` ms; returns the first that appears or null on timeout. The
 * paper-trading panels (blotter / positions / account / reconciliation) ship
 * after the signal cockpit; specs use this to detect whether a panel is built
 * yet and self-skip cleanly if not.
 */
export async function firstVisibleTestId(
  page: Page,
  testids: string[],
  timeout = 10_000,
): Promise<string | null> {
  const deadline = Date.now() + timeout;
  while (Date.now() < deadline) {
    for (const id of testids) {
      const loc = page.getByTestId(id).first();
      if ((await loc.count()) && (await loc.isVisible().catch(() => false))) {
        return id;
      }
    }
    await page.waitForTimeout(200);
  }
  return null;
}

/**
 * Poll `read` until it returns a value satisfying `pred`, or until `timeout`.
 * Returns the last value observed. Used to wait for a paper order to reach a
 * terminal state (FILLED), a position count to settle, etc. — DB- or UI-backed.
 */
export async function waitFor<T>(
  read: () => Promise<T>,
  pred: (v: T) => boolean,
  opts: { interval?: number; timeout?: number } = {},
): Promise<T> {
  const interval = opts.interval ?? 1_000;
  const timeout = opts.timeout ?? 30_000;
  const deadline = Date.now() + timeout;
  let last = await read();
  if (pred(last)) return last;
  while (Date.now() < deadline) {
    await new Promise((r) => setTimeout(r, interval));
    last = await read();
    if (pred(last)) return last;
  }
  return last;
}

/**
 * Poll until the streaming live-intent count seen by the cockpit stops growing,
 * i.e. two consecutive reads `apart` ms apart return the same value. Returns the
 * settled count. Used by the kill-switch spec to prove no NEW intents appear
 * after a halt. `read` returns the current observed count (DB or UI).
 */
export async function waitIntentCountStable(
  read: () => Promise<number>,
  opts: { apart?: number; timeout?: number } = {},
): Promise<number> {
  const apart = opts.apart ?? 3_000;
  const timeout = opts.timeout ?? 30_000;
  const deadline = Date.now() + timeout;
  let prev = await read();
  while (Date.now() < deadline) {
    await new Promise((r) => setTimeout(r, apart));
    const cur = await read();
    if (cur === prev) return cur;
    prev = cur;
  }
  return prev;
}

/** Parse a numeric data-* attribute (the cockpit exposes a live intent count as
 * `data-intent-count` on its stream panel for deterministic assertions). Returns
 * null when the attribute is missing or non-numeric. */
export function parseCount(raw: string | null): number | null {
  if (raw == null) return null;
  const n = Number(raw.trim());
  return Number.isFinite(n) ? n : null;
}

// ---------------------------------------------------------------------------
// Manual trading desk (P6, operator-driven). The desk lets the operator place /
// cancel / close BY HAND against a paper or live account, in ANY strategy mode
// (signal: the operator IS the executor; paper/live: an override alongside the
// auto book). It reuses the MoomooExecutor + Trd_* client + order-state machine
// + tms.orders/fills/positions + the mock venue, attributing every manual order
// to the `MANUAL` pseudo-strategy so reconciliation + per-strategy accounting
// stay clean (docs/api.md "Manual trading desk").
//
// SAFETY (paramount — this can place REAL orders): a paper manual order needs the
// trade password in confirm_token; a LIVE manual order needs the full 4-factor
// activation PLUS a per-order typed confirmation phrase. The gate runs the desk
// in PAPER over the mock venue; the safety specs prove the live guard EXISTS
// without ever placing a real order.
//
// These helpers gate the manual-desk specs the same way the paper-trading specs
// gate on a paper session: they self-skip cleanly until the desk + the cockpit's
// manual panels land, so the gate stays green meanwhile (the established
// specs-07-17 pattern).
// ---------------------------------------------------------------------------

/** The pseudo-strategy id every manual order/position is booked under (distinct
 * from the auto strategies so reconciliation + per-strategy accounting stay
 * clean — docs/api.md "Manual trading desk"). */
export const MANUAL_STRATEGY_ID = "MANUAL";

/** The exact per-order confirmation phrase a LIVE (real-money) manual order
 * requires in `confirm_token` (docs/api.md). A near-miss must NEVER arm a real
 * order — the safety specs assert the boundary holds with the wrong phrase. */
export const MANUAL_LIVE_CONFIRM_PHRASE = "I CONFIRM THIS REAL MONEY MANUAL ORDER";

/**
 * Whether a MANUAL trading desk is connected. We probe GET /api/v1/trade/status
 * — a DEDICATED desk-status endpoint on the ACTUAL mutation surface: 503 when no
 * desk is attached (the live node was started without `--manual-mode`, or the
 * desk has not finished connecting), 200 {connected,mode,live} once a desk is
 * bound. We must NOT probe GET /api/v1/trade/account for this: that route reuses
 * the always-present live-account reader and returns 200 EVEN WITH NO DESK
 * connected, so the skip-guard would pass and the specs would then HARD-FAIL on
 * the real /trade/* POSTs returning 503 (the original gate bug). /trade/status
 * is gated on the desk itself, so a 200 here guarantees the mutation POSTs work.
 */
export async function manualDeskAvailable(): Promise<boolean> {
  const res = await getManual("trade/status");
  return res.status === 200;
}

/**
 * Whether a connected manual desk is bound to a PAPER account (never live). The
 * gate only ever runs the desk in paper over the mock venue; the order-placing
 * specs gate on this so they NEVER place against a real/live account. The desk's
 * account view exposes its mode; absent that, we fall back to the session mode
 * (the desk attaches to the live node, whose session is signal/paper in the
 * gate). Returns true only when we can positively confirm paper (or signal — the
 * operator-as-executor case, still the mock venue), never live.
 */
export async function manualDeskIsPaper(): Promise<boolean> {
  const res = await getManual("trade/status");
  if (res.status !== 200) return false;
  const body = res.body as { mode?: string; live?: boolean } | undefined;
  // A live desk is NEVER paper — refuse outright (never place against live).
  if (body?.live === true || body?.mode === "live") return false;
  if (body?.mode) return body.mode === "paper" || body.mode === "signal";
  // No explicit desk mode surfaced — defer to the session the desk attaches to.
  const s = await currentSession();
  return !!s && (s.mode === "paper" || s.mode === "signal");
}

/**
 * Whether a manual desk can SYNC from the broker (DIRECTION 2, broker -> TMS).
 * `POST /api/v1/trade/sync` is READ-ONLY at the broker (places NO orders) and is
 * safe in ALL modes incl signal, so — unlike the order-placing helpers — it does
 * NOT gate on `manualDeskIsPaper()`. It only needs a manual desk connected; the
 * endpoint returns 503 when none is. We treat any non-503 as "sync available".
 */
export async function manualSyncAvailable(): Promise<boolean> {
  // A connected desk is the only precondition; reuse the account probe (the
  // cheapest non-mutating 503-vs-present signal) rather than POSTing a sync here.
  return manualDeskAvailable();
}

/**
 * True once the MANUAL trading desk is rendered. Post-restructure the desk is
 * the Desk view of the trade module, reached via `/paper?view=desk` (the old
 * `/trade/desk` route now 301-redirects to `/paper`). Its root testid
 * (`manual-desk`) is rendered by <ManualDesk> when the module's `?view=desk`
 * sub-nav is active (ui/src/components/portfolio/trade-module.tsx +
 * desk/manual-desk.tsx). Returns false when the app-shell or the desk root never
 * appears (route not built / not implemented).
 */
export async function manualDeskUiReady(page: Page): Promise<boolean> {
  await page.goto("/paper?view=desk", { waitUntil: "domcontentloaded" });
  const shell = page.getByTestId("app-shell");
  try {
    await shell.waitFor({ state: "visible", timeout: 15_000 });
  } catch {
    return false;
  }
  const real = page.getByTestId("manual-desk");
  try {
    await real.waitFor({ state: "visible", timeout: 15_000 });
    return true;
  } catch {
    return false;
  }
}
