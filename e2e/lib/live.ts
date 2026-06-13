/**
 * Live cockpit e2e helpers (P5).
 *
 * The /live cockpit is the read surface over the live trading node: signal
 * intents streaming in over the WS (bridged from Redis, durable truth in
 * tms.signal_intents), portfolio health + session state, the watchlist
 * (tracked universe), and the audited control surface (halt / kill-switch /
 * mode-switch via POST /api/v1/live/commands). See docs/api.md "Live (P5)" and
 * docs/spec/{ui-runner-modes-eod,portfolio-risk}.md.
 *
 * GATE TOPOLOGY (P5 decisions 2/3/7): the gate runs `tms-live --mode signal`
 * against the in-repo MOCK OpenD server, which replays a day of bars out of our
 * Postgres — so a signal session emits intents into tms.signal_intents + the
 * Redis streams the API bridges to WS, with NO real OpenD. The real-OpenD smoke
 * is deferred to market hours (docs/runbooks/live-smoke.md).
 *
 * BUILD ORDER: the /live cockpit + the live read endpoints land in P5, after
 * the P1 Data / P2 Backtests / P3 Strategies+Hyperopt workspaces. These specs
 * are PERMANENT and assert the documented contract; while the section is still
 * the coming-soon placeholder, the live reader is absent (endpoints 503), or no
 * signal session has run yet, they self-skip cleanly so the gate stays green —
 * exactly like specs 07-17 did for their workspaces before they landed.
 *
 * Ground truth is read independently from postgres (lib/db) and the Go API
 * (lib/api) — the UI renders the UI's proxy of the API, and both must agree
 * with the DB. No fabricated values.
 */

import type { Page } from "@playwright/test";
import { getAuthed } from "./api";
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
 * True once the real /live cockpit replaced the coming-soon placeholder.
 *
 * Mirrors backtestsUiReady / strategiesUiReady: the cockpit root (`live-page`)
 * only exists in the real workspace; the placeholder (`live-placeholder`, the
 * current ComingSoon testid) marks coming-soon. Returns false when neither has
 * appeared (route not built at all) or the placeholder is still showing.
 */
export async function liveUiReady(page: Page): Promise<boolean> {
  await page.goto("/live", { waitUntil: "domcontentloaded" });
  const shell = page.getByTestId("app-shell");
  try {
    await shell.waitFor({ state: "visible", timeout: 15_000 });
  } catch {
    return false;
  }
  const real = page.getByTestId("live-page");
  const placeholder = page.getByTestId("live-placeholder");
  const deadline = Date.now() + 15_000;
  while (Date.now() < deadline) {
    if (await real.count()) return true;
    if (await placeholder.count()) return false;
    await page.waitForTimeout(250);
  }
  return false;
}

/**
 * Whether the API was started WITH a live reader. The live read endpoints
 * return 503 when the API has no live reader configured (docs/api.md "Live
 * (P5)": "The live read endpoints return 503 when the API was started without a
 * live reader."). A 200 OR a `{session:null}` 200 means the reader is present;
 * a 503 means the live surface is unavailable and the cockpit specs must skip.
 *
 * We probe GET /api/v1/live/session: 503 → no reader; anything else (200 with a
 * session or {session:null}, even 404-ish) → reader present.
 */
export async function liveReaderAvailable(): Promise<boolean> {
  const res = await getAuthed("live/session");
  return res.status !== 503;
}

/** The most recent session, or null. Used to gate session-dependent assertions
 * (a signal session must have run for intents/health/watchlist to be populated). */
export async function currentSession(): Promise<LiveSessionTruth | null> {
  return withDb((c) => latestSession(c));
}

/** A RUNNING signal session exists (the gate's `tms-live --mode signal` node).
 * Several specs require a live emitter to be running; they skip otherwise. */
export async function hasRunningSignalSession(): Promise<boolean> {
  const s = await currentSession();
  return !!s && s.mode === "signal" && s.status === "RUNNING";
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
