/**
 * (4) Live cockpit — KILL-SWITCH / halt.
 *
 * The cockpit's halt control is the audited side channel for the trading
 * mutation surface: clicking it enqueues a `halt` (or `kill`) command via
 * POST /api/v1/live/commands, which the tms-live consumer applies idempotently —
 * it STOPS emitting NEW intents and sets the halt state, writing a tms.halts row
 * (docs/api.md "Live (P5)"; P5 decision 6; FLATTEN deferred to P6).
 *
 * This spec proves, end to end through the UI:
 *   - clicking halt opens a confirmation, and confirming fires the command;
 *   - a new tms.halts row appears (durable audit) and the active-halt count > 0;
 *   - the cockpit reflects the halted state (a halt banner / data-halted flag);
 *   - NO NEW intents appear after the halt — the streaming intent count stops
 *     growing (two consecutive reads settle to the same value).
 *
 * Because halting mutates shared session state, this spec is gated hard: it
 * runs ONLY when a RUNNING signal session exists, the cockpit is built, and the
 * live reader is present; otherwise it self-skips. It is placed last among the
 * live specs (the suite runs serially, workers:1) so it does not starve the
 * read-only cockpit specs of a live emitter.
 */

import { test, expect } from "../fixtures/test";
import {
  withDb,
  streamingIntentCount,
  activeHaltCount,
  haltRowCount,
} from "../lib/db";
import {
  liveUiReady,
  liveReaderAvailable,
  hasRunningSignalSession,
  waitIntentCountStable,
} from "../lib/live";

test.describe("live cockpit — kill-switch / halt", () => {
  test("halt confirmation stops new intents and writes a halts row", async ({
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
    if (!(await hasRunningSignalSession())) {
      test.skip(
        true,
        "no RUNNING signal session — nothing to halt (read-only stack).",
      );
      return;
    }

    await expect(page.getByTestId("live-page")).toBeVisible();

    const before = await withDb(async (c) => ({
      halts: await haltRowCount(c),
      activeHalts: await activeHaltCount(c),
      intents: await streamingIntentCount(c),
    }));

    // 1. Fire the kill-switch / halt control. The cockpit guards a halt behind a
    //    confirmation step (a destructive control), exposed as `live-halt-button`
    //    opening a `live-halt-confirm` dialog with a `live-halt-confirm-submit`.
    const haltButton = page.getByTestId("live-halt-button");
    await expect(haltButton).toBeVisible({ timeout: 15_000 });
    await haltButton.click();

    const confirm = page.getByTestId("live-halt-confirm");
    await expect(confirm).toBeVisible();
    // The confirmation makes the operator state intent (a reason is optional per
    // the command contract, but the cockpit may require one). Fill if present.
    const reason = page.getByTestId("live-halt-reason");
    if (await reason.count()) {
      await reason.fill("e2e kill-switch");
    }
    // The destructive halt is guarded behind a typed-confirmation phrase ("HALT"):
    // the submit button stays disabled until the operator types the exact phrase
    // (a deliberate kill-switch safety, mirrored by the mode-switch dialog in
    // spec 22). Arm the action by typing the phrase, then submit.
    const phrase = page.getByTestId("live-halt-confirm-phrase");
    if (await phrase.count()) {
      await phrase.fill("HALT");
    }
    const submit = page.getByTestId("live-halt-confirm-submit");
    await expect(submit).toBeEnabled();
    await submit.click();

    // 2. The command was accepted (202). The cockpit surfaces the enqueued
    //    command (a pending indicator / id); we then confirm via durable truth.
    //    A new tms.halts row appears (the consumer applies it under audit).
    await expect
      .poll(async () => withDb((c) => haltRowCount(c)), { timeout: 30_000 })
      .toBeGreaterThanOrEqual(before.halts + 1);

    // An ACTIVE halt now exists (cleared_at NULL) — the session is halted.
    await expect
      .poll(async () => withDb((c) => activeHaltCount(c)), { timeout: 30_000 })
      .toBeGreaterThanOrEqual(before.activeHalts + 1);

    // 3. The cockpit reflects the halted state. The session/health strip exposes
    //    a halted flag and/or a visible banner.
    const halted = page.getByTestId("live-halted-banner");
    const sessionStrip = page.getByTestId("live-session");
    await expect
      .poll(
        async () => {
          if ((await halted.count()) && (await halted.first().isVisible())) {
            return true;
          }
          const flag = await sessionStrip.getAttribute("data-halted");
          return flag === "true";
        },
        { timeout: 30_000 },
      )
      .toBe(true);

    // 4. No NEW intents appear after the halt. The consumer stops emitting; the
    //    streaming intent count must STOP GROWING — two consecutive reads, a few
    //    seconds apart, settle to the same value. We first let any in-flight bar
    //    drain, then assert stability.
    const settled = await waitIntentCountStable(
      () => withDb((c) => streamingIntentCount(c)),
      { apart: 3_000, timeout: 30_000 },
    );

    // Cross-check: a second window after the settle yields the SAME count — the
    // emitter is genuinely halted, not merely momentarily idle between bars.
    await page.waitForTimeout(4_000);
    const finalCount = await withDb((c) => streamingIntentCount(c));
    expect(
      finalCount,
      "streaming intent count is frozen after the halt (no new intents)",
    ).toBe(settled);

    // And it never went backwards (append-only) relative to the pre-halt count.
    expect(finalCount).toBeGreaterThanOrEqual(before.intents);
  });
});
