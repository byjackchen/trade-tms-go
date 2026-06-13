/**
 * (1) Backtest launch flow.
 *
 * Open the New-backtest dialog, fill a small scripted backtest (two seeded/real
 * tickers, ~3-month window, nautilus-compat fill profile, one LONG intent),
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

/** True once the real Backtests workspace replaced the coming-soon placeholder. */
async function backtestsUiReady(
  page: import("@playwright/test").Page,
): Promise<boolean> {
  await page.goto("/backtests", { waitUntil: "domcontentloaded" });
  await expect(page.getByTestId("app-shell")).toBeVisible();
  // The launch affordance only exists in the real workspace.
  const placeholder = page.getByTestId("backtests-placeholder");
  const launcher = page.getByTestId("open-backtest-dialog");
  await expect
    .poll(
      async () => (await placeholder.count()) + (await launcher.count()),
      { timeout: 15_000 },
    )
    .toBeGreaterThan(0);
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

    // 1. Open the New-backtest dialog.
    await page.getByTestId("open-backtest-dialog").click();
    const dialog = page.getByTestId("backtest-dialog");
    await expect(dialog).toBeVisible();
    await expect(page.getByTestId("backtest-form")).toBeVisible();

    // 2. Fill the scripted backtest: two tickers, the covered window, the
    //    parity (zero-cost) fill profile.
    await page
      .getByTestId("backtest-tickers")
      .fill(launch.tickers.join(" "));
    await page.getByTestId("backtest-start").fill(launch.start);
    await page.getByTestId("backtest-end").fill(launch.end);
    // The fill profile is a select with the nautilus-compat option as default,
    // but set it explicitly so the run is deterministic regardless of default.
    const profile = page.getByTestId("backtest-fill-profile");
    if (await profile.count()) {
      await profile.selectOption("nautilus-compat").catch(() => {
        /* default already nautilus-compat; some builds render it read-only. */
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
    //    run id (no fabricated id). The UI either redirects automatically or
    //    exposes a `backtest-detail-link`.
    const detailLink = page.getByTestId("backtest-detail-link");
    if (await detailLink.count()) {
      await detailLink.first().click();
    }
    await expect
      .poll(async () => new URL(page.url()).pathname, { timeout: 30_000 })
      .toMatch(/\/backtests\/\d+$/);

    const urlId = Number(
      new URL(page.url()).pathname.replace(/.*\/backtests\//, ""),
    );
    expect(
      urlId,
      "detail URL points at the run the engine just persisted",
    ).toBe(latest!.id);

    // The detail page mounted its summary header.
    await expect(page.getByTestId("backtest-detail")).toBeVisible({
      timeout: 30_000,
    });
  });
});
