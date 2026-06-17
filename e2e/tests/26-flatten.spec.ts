/**
 * (3) FLATTEN — close ALL open positions via the confirmation-gated control.
 *
 * In paper/live, the `flatten` command submits FLAT market orders that close
 * every open position, idempotently (P6 decision 7, deferred from P5). It is a
 * destructive mutation, so it requires a typed confirmation phrase whose value
 * is the `confirm_token` the API demands (docs/api.md: `flatten` →
 * `confirm_token` required; 412 confirmation_required without it). FLAT/closing
 * orders BYPASS the portfolio budget gate by design, and flatten is allowed even
 * during a halt (the whole point of a kill/flatten).
 *
 * Two independent checks:
 *   (a) API GUARD (always safe to run with a trading reader — a 412 means NO
 *       flatten happened): `flatten` WITHOUT a confirm_token is rejected 412
 *       confirmation_required. This proves the boundary guard exists even before
 *       the UI control ships.
 *   (b) UI FLOW (when the cockpit's flatten control + a non-empty position book
 *       exist): click flatten -> a confirmation-phrase dialog opens, submit is
 *       disabled until the phrase is typed; arming + submitting fires the command
 *       and the open-position count drops to ZERO (qty -> 0), with FLAT/close
 *       orders appearing in the blotter. We only complete the flatten when a
 *       paper session is running (never against a real/live account).
 *
 * Testid contract:
 *   live-flatten-button         — opens the flatten confirmation
 *   live-flatten-confirm        — the confirmation dialog
 *   live-flatten-confirm-phrase — the typed-phrase input
 *   live-flatten-confirm-submit — the arm/submit button (disabled until armed)
 *   live-flatten-confirm-cancel — cancel without flattening
 *   live-positions / live-position-row — the open-position book
 */

import { test, expect } from "../fixtures/test";
import { postAuthed } from "../lib/api";
import {
  withDb,
  latestSession,
  openPositionCount,
} from "../lib/db";
import {
  liveUiReady,
  liveTradingAvailable,
  hasRunningPaperSession,
  firstVisibleTestId,
  waitFor,
} from "../lib/live";

test.describe("paper trading — flatten closes all positions", () => {
  test("the API rejects a flatten without a confirm token (412)", async () => {
    if (!(await liveTradingAvailable())) {
      test.skip(
        true,
        "API started without a trading reader (live trading endpoints 503).",
      );
      return;
    }

    const before = await withDb((c) => latestSession(c));

    // flatten WITHOUT confirm_token must be rejected at the boundary — a denied
    // confirmation means NO positions were touched (safety: a fat-fingered
    // flatten cannot fire without the typed phrase).
    const res = await postAuthed("trade/commands", {
      name: "flatten",
      reason: "e2e guard check (no token)",
    });
    expect(
      res.status,
      "flatten without confirm_token is a confirmation-required 412",
    ).toBe(412);
    const body = res.body as { error?: { code?: string } } | undefined;
    expect(body?.error?.code).toBe("confirmation_required");

    // The guard means nothing changed: the latest session is unchanged.
    const after = await withDb((c) => latestSession(c));
    if (before && after) {
      expect(after.id, "the rejected flatten did not start/stop a session").toBe(
        before.id,
      );
    }
  });

  test("the cockpit flatten control closes every open position", async ({
    page,
  }) => {
    if (!(await liveUiReady(page))) {
      test.skip(true, "Live cockpit not yet implemented (coming-soon).");
      return;
    }
    if (!(await liveTradingAvailable())) {
      test.skip(
        true,
        "API started without a trading reader (live trading endpoints 503).",
      );
      return;
    }
    // SAFETY: only ever complete a flatten against a PAPER session — never live.
    if (!(await hasRunningPaperSession())) {
      test.skip(
        true,
        "no RUNNING paper session — refusing to complete a flatten (never against live).",
      );
      return;
    }

    // Post-restructure the cockpit is the trade module at /paper; its ready
    // signal is the unified `trade-header` testid (the old paper/live page roots are retired).
    await expect(page.getByTestId("trade-header")).toBeVisible();

    const session = await withDb((c) => latestSession(c));
    const sessionId = session!.id;

    const flattenButton = await firstVisibleTestId(
      page,
      ["live-flatten-button"],
      10_000,
    );
    if (!flattenButton) {
      test.skip(
        true,
        "flatten control not surfaced yet (paper-trading cockpit panels deferred).",
      );
      return;
    }

    // Wait (bounded) for the paper session to actually open positions, otherwise
    // there is nothing to flatten this run.
    const openBefore = await waitFor(
      () => withDb((c) => openPositionCount(c, sessionId)),
      (n) => n > 0,
      { interval: 1_500, timeout: 45_000 },
    );
    if (openBefore === 0) {
      test.skip(
        true,
        "no open positions to flatten this session (no setup emitted).",
      );
      return;
    }

    // Open the flatten confirmation. Submit must be DISABLED until the exact
    // phrase is typed (the destructive-action guard, mirroring halt/kill/mode).
    await page.getByTestId("live-flatten-button").click();
    const dialog = page.getByTestId("live-flatten-confirm");
    await expect(dialog).toBeVisible();

    const phrase = page.getByTestId("live-flatten-confirm-phrase");
    await expect(phrase).toBeVisible();
    const submit = page.getByTestId("live-flatten-confirm-submit");
    const disabledBefore =
      (await submit.isDisabled().catch(() => false)) ||
      (await submit.getAttribute("aria-disabled")) === "true";
    expect(
      disabledBefore,
      "flatten submit is disabled until the confirmation phrase is typed",
    ).toBeTruthy();

    // Arm with the documented flatten phrase AND an audited reason. The flatten
    // dialog requires BOTH a reason (audited) and the exact phrase before submit
    // enables — mirroring halt/kill. FLATTEN is the canonical phrase.
    await page.getByTestId("flatten-reason").fill("e2e flatten — close book");
    await phrase.fill("FLATTEN");
    await expect(submit).toBeEnabled({ timeout: 5_000 });
    await submit.click();

    // The command was accepted; the consumer submits FLAT market orders closing
    // every open position. Durable truth: the open-position count drops to ZERO.
    await expect
      .poll(async () => withDb((c) => openPositionCount(c, sessionId)), {
        timeout: 45_000,
      })
      .toBe(0);

    // The positions panel reflects the now-flat book (zero open rows). The panel
    // may keep closed rows; we assert no row reports a non-zero signed qty.
    const positionsPanel = await firstVisibleTestId(page, ["live-positions"], 8_000);
    if (positionsPanel) {
      const rows = page.getByTestId("live-position-row");
      await expect
        .poll(
          async () => {
            const n = await rows.count();
            for (let i = 0; i < n; i++) {
              const sq = await rows.nth(i).getAttribute("data-signed-qty");
              if (sq != null && Number(sq) !== 0) return false;
            }
            return true;
          },
          { timeout: 20_000 },
        )
        .toBe(true);
    }

    // FLAT/close orders appear in the blotter (the flatten submitted real orders).
    const blotter = await firstVisibleTestId(page, ["live-blotter"], 8_000);
    if (blotter) {
      const orderRows = page.getByTestId("live-blotter-order-row");
      await expect
        .poll(async () => orderRows.count(), { timeout: 15_000 })
        .toBeGreaterThan(0);
    }
  });
});
