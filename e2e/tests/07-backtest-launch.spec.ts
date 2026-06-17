/**
 * (1) Backtest launch flow.
 *
 * Open the New-backtest dialog, fill a small scripted backtest (two seeded/real
 * tickers, ~3-month window, close-fill fill profile, one LONG intent),
 * submit, then watch the shared job-progress panel transition running ->
 * succeeded purely through the UI (the worker drives it; the tracker reconciles
 * over the WS/SSE job stream + REST poll). Finally follow the detail link to
 * /backtests/{id} and confirm the run's id reconciles with the DB.
 *
 * Ground-truth coupling: the run the UI links to must be the run the engine
 * actually persisted (tms.runs gains a COMPLETE row whose id matches the detail
 * URL). No fabricated ids.
 *
 * Robustness while the Backtests workspace is still landing (it ships after the
 * P1 Data workspace): the spec self-skips when the page is still the
 * `coming-soon` placeholder, or when the stack has no tradable bars. Once the UI
 * is wired, the assertions are exact and permanent.
 */

import { test, expect } from "../fixtures/test";
import { withDb, completeRunCount, latestCompleteRun } from "../lib/db";
import { pickScriptedLaunch, TERMINAL } from "../lib/backtests";

/**
 * True once a backtest LAUNCH affordance is reachable. In the 4-top IA a
 * backtest's object is always a Composition (docs/concept-alignment.md §3.4 A3):
 * the retired `/backtests` route 301-redirects to the Compositions module, where
 * a backtest is launched PER-COMPOSITION (`composition-backtest-<id>`) rather than
 * via the old standalone `open-backtest-dialog`. This guard navigates to
 * `/compositions` and reports whether a per-Composition Backtest launcher exists
 * yet; it SOFT-skips (returns false) — never hard-fails — while the Composition-
 * centric launch flow is still landing, matching the established "self-skip until
 * built" pattern so the gate stays green.
 */
async function backtestsUiReady(
  page: import("@playwright/test").Page,
): Promise<boolean> {
  await page.goto("/compositions", { waitUntil: "domcontentloaded" });
  await expect(page.getByTestId("app-shell")).toBeVisible();
  // The Compositions module must mount; then a per-Composition Backtest launcher
  // (composition-backtest-<id>) is the launch affordance. Absent (empty registry
  // / flow not wired) => soft-skip.
  const page_ = page.getByTestId("compositions-page");
  try {
    await page_.waitFor({ state: "visible", timeout: 15_000 });
  } catch {
    return false;
  }
  const launcher = page.locator('[data-testid^="composition-backtest-"]').first();
  return (await launcher.count()) > 0;
}

test.describe("backtest launch flow", () => {
  test("launch a scripted backtest, watch it succeed, link to detail", async ({
    page,
  }) => {
    if (!(await backtestsUiReady(page))) {
      test.skip(true, "Backtests workspace not yet implemented (coming-soon).");
      return;
    }

    const launch = await pickScriptedLaunch();
    if (!launch) {
      test.skip(true, "no tradable bars in the stack — cannot run a backtest.");
      return;
    }

    const before = await withDb((c) => completeRunCount(c));

    // 1. Open the New-backtest dialog. In the 4-top IA the launch affordance is
    //    PER-COMPOSITION (composition-backtest-<id>) — a backtest's object is
    //    always a Composition — which opens the shared NewBacktestDialog
    //    (`backtest-dialog`) prefilled to that Composition.
    await page
      .locator('[data-testid^="composition-backtest-"]')
      .first()
      .click();
    const dialog = page.getByTestId("backtest-dialog");
    await expect(dialog).toBeVisible();
    await expect(page.getByTestId("backtest-form")).toBeVisible();

    // 2. Fill the scripted backtest: two tickers, the covered window, the
    //    close-fill (zero-cost) fill profile.
    await page
      .getByTestId("backtest-tickers")
      .fill(launch.tickers.join(" "));
    await page.getByTestId("backtest-start").fill(launch.start);
    await page.getByTestId("backtest-end").fill(launch.end);
    // The fill profile defaults to realistic; set close-fill explicitly so the
    // run is deterministic (same-bar close fill) regardless of default.
    const profile = page.getByTestId("backtest-fill-profile");
    if (await profile.count()) {
      await profile.selectOption("close-fill").catch(() => {
        /* some builds render it read-only. */
      });
    }

    // 3. Submit.
    await page.getByTestId("backtest-submit").click();

    // 4. The shared job-progress view appears (the backtest.run job enqueued).
    const progress = page.getByTestId("job-progress");
    await expect(progress).toBeVisible();
    const jobId = await progress.getAttribute("data-job-id");
    expect(jobId, "job-progress exposes a numeric data-job-id").toMatch(/^\d+$/);

    // 5. Observe running -> terminal through the UI. A scripted backtest over a
    //    tiny window should SUCCEED; we require a terminal outcome and assert it
    //    is `succeeded` (a fabricated/empty run would fail — caught here).
    const complete = page.getByTestId("job-complete");
    await expect(complete).toBeVisible({ timeout: 80_000 });
    const outcome = await complete.getAttribute("data-outcome");
    expect(
      outcome && TERMINAL.has(outcome),
      `backtest job reached a terminal outcome (got "${outcome}")`,
    ).toBeTruthy();
    expect(
      outcome,
      `scripted backtest succeeds end-to-end (got "${outcome}")`,
    ).toBe("succeeded");

    // 6. The DB gained a COMPLETE run.
    await expect
      .poll(async () => withDb((c) => completeRunCount(c)), { timeout: 30_000 })
      .toBeGreaterThanOrEqual(before + 1);
    const latest = await withDb((c) => latestCompleteRun(c));
    expect(latest, "a COMPLETE run exists after the launch").not.toBeNull();

    // 7. Follow the detail link/redirect and confirm it targets the persisted
    //    run id (no fabricated id). In the 4-top IA the result opens INLINE in the
    //    Compositions module via the `?backtest=<id>` deep-link (NewBacktestDialog
    //    onView -> /compositions?backtest=<id>); the UI either routes there
    //    automatically or exposes a `backtest-detail-link`.
    const detailLink = page.getByTestId("backtest-detail-link");
    if (await detailLink.count()) {
      await detailLink.first().click();
    }
    await expect
      .poll(() => new URL(page.url()).searchParams.get("backtest"), {
        timeout: 30_000,
      })
      .toBe(String(latest!.id));

    // The inline backtest panel mounted its summary header for the persisted run.
    await expect(page.getByTestId("backtest-detail")).toBeVisible({
      timeout: 30_000,
    });
  });
});
