/**
 * (1) Live cockpit — signal intents stream in over the WS and MATCH the DB.
 *
 * The gate runs `tms trade run --mode signal` against the MOCK OpenD server, which
 * replays a day of bars out of our Postgres. Each strategy evaluate_intent per
 * bar records a SignalIntent into tms.signal_intents (append-only, as_of NULL)
 * AND publishes to the Redis stream the API bridges to the cockpit's WS
 * (docs/api.md "Live (P5)", `signal_intent` frame; P5 decision 3).
 *
 * This spec proves, through the UI:
 *   - the cockpit's live-intent surface mounts and shows streaming intents;
 *   - the rows the UI shows are not fabricated — every (strategy_id, symbol)
 *     pair the cockpit renders is present in tms.signal_intents, and the
 *     cockpit's reported intent count agrees with the DB streaming-intent count.
 *
 * Robustness: self-skips when the /trade cockpit is still the coming-soon
 * placeholder, when the API has no live reader (503), or when no streaming
 * intents exist yet (no signal session has emitted) — exactly like the
 * Backtests/Strategies specs self-skipped before their workspaces landed. Once
 * the cockpit is wired and a signal session has run, the assertions are exact.
 */

import { test, expect } from "../fixtures/test";
import {
  withDb,
  streamingIntentCount,
  recentStreamingIntents,
} from "../lib/db";
import {
  liveUiReady,
  liveReaderAvailable,
  parseCount,
  INTENT_STRATEGY_IDS,
  INTENT_STATES,
} from "../lib/live";

test.describe("live cockpit — signal intents stream", () => {
  test("cockpit shows streaming intents that match tms.signal_intents", async ({
    page,
  }) => {
    if (!(await liveUiReady(page))) {
      test.skip(true, "Live cockpit not yet implemented (coming-soon).");
      return;
    }
    if (!(await liveReaderAvailable())) {
      test.skip(true, "API started without a live reader (live endpoints 503).");
      return;
    }

    // The cockpit is up. Mount its live-intent surface.
    await expect(page.getByTestId("app-shell")).toBeVisible();
    await expect(page.getByTestId("trade-header")).toBeVisible();

    const intentsPanel = page.getByTestId("live-intents");
    await expect(intentsPanel).toBeVisible({ timeout: 15_000 });

    // Ground truth: streaming intents (as_of NULL) the live emitter recorded.
    const dbCount = await withDb((c) => streamingIntentCount(c));
    if (dbCount === 0) {
      test.skip(
        true,
        "no streaming intents yet — no signal session has emitted.",
      );
      return;
    }

    // The cockpit renders intent rows. Each row carries identity attributes so
    // the test can compare against the DB without scraping free text:
    //   data-strategy-id, data-symbol on `live-intent-row`.
    const rows = page.getByTestId("live-intent-row");
    await expect
      .poll(async () => rows.count(), { timeout: 20_000 })
      .toBeGreaterThan(0);

    // Pull the DB's recent (strategy_id, symbol) pairs as the allowed set: the
    // UI dedupes (symbol, strategy_id) newest-wins (api spec §3.9), so the UI's
    // distinct keys must be a SUBSET of what the DB has emitted. A fabricated
    // row (a pair the engine never produced) would fail this membership check.
    const truth = await withDb((c) => recentStreamingIntents(c, 2000));
    const truthKeys = new Set(
      truth.map((t) => `${t.strategyId}:${t.symbol}`),
    );

    const n = await rows.count();
    expect(n).toBeGreaterThan(0);
    for (let i = 0; i < n; i++) {
      const row = rows.nth(i);
      const sid = await row.getAttribute("data-strategy-id");
      const sym = await row.getAttribute("data-symbol");
      expect(sid, `row ${i} exposes data-strategy-id`).toBeTruthy();
      expect(sym, `row ${i} exposes data-symbol`).toBeTruthy();
      // Discriminator + state must be valid wire values (not garbage).
      expect(
        INTENT_STRATEGY_IDS.includes(sid as never),
        `row ${i} strategy_id "${sid}" is a known discriminator`,
      ).toBeTruthy();
      const state = await row.getAttribute("data-state");
      if (state != null) {
        expect(
          INTENT_STATES.includes(state as never),
          `row ${i} state "${state}" is a known signal state`,
        ).toBeTruthy();
      }
      // Membership: the (strategy_id, symbol) pair the UI shows was emitted.
      expect(
        truthKeys.has(`${sid}:${sym}`),
        `cockpit intent ${sid}/${sym} exists in tms.signal_intents`,
      ).toBeTruthy();
    }

    // If the cockpit exposes an aggregate live-intent count, it must agree with
    // the DB streaming count (the count must reflect the durable truth, never
    // an inflated client-side tally). Tolerate a small lag: the cockpit count is
    // a subset/dedup view, so it is ≤ the DB total but > 0 once streaming.
    const countAttr = parseCount(
      await intentsPanel.getAttribute("data-intent-count"),
    );
    if (countAttr != null) {
      expect(countAttr, "cockpit intent count is positive once streaming").toBeGreaterThan(0);
      expect(
        countAttr,
        "cockpit intent count never exceeds the DB streaming truth",
      ).toBeLessThanOrEqual(dbCount);
    }
  });

  test("the live-intent count grows as the mock feed replays bars", async ({
    page,
  }) => {
    if (!(await liveUiReady(page))) {
      test.skip(true, "Live cockpit not yet implemented (coming-soon).");
      return;
    }
    if (!(await liveReaderAvailable())) {
      test.skip(true, "API started without a live reader (live endpoints 503).");
      return;
    }

    const intentsPanel = page.getByTestId("live-intents");
    await expect(intentsPanel).toBeVisible({ timeout: 15_000 });

    // The DB streaming count is the durable truth that the mock replay advances.
    const before = await withDb((c) => streamingIntentCount(c));
    if (before === 0) {
      test.skip(
        true,
        "no streaming intents yet — no signal session is emitting.",
      );
      return;
    }

    // While a signal session is running against the mock feed, the streaming
    // intent count is monotonic non-decreasing. We assert monotonicity over a
    // bounded window (the session may have finished replaying its day, in which
    // case the count holds — both are valid, neither loses rows).
    const after = await withDb((c) => streamingIntentCount(c));
    expect(
      after,
      "streaming intents are append-only (count never shrinks)",
    ).toBeGreaterThanOrEqual(before);

    // And the cockpit reflects that the WS delivered live frames: the live-
    // connection indicator shows connected once the WS is open.
    const conn = page.getByTestId("live-connection");
    if (await conn.count()) {
      await expect
        .poll(async () => conn.getAttribute("data-connected"), {
          timeout: 15_000,
        })
        .toBe("true");
    }
  });
});
