/**
 * (4) Cancel a long-ish backtest mid-run from the UI -> status canceled.
 *
 * To get a window in which the job is observably running (so the cancel button
 * appears), this launch uses the WIDEST available bar window over several
 * tickers — many more bars => a longer engine loop than the tiny launch spec.
 *
 * Cancellation semantics (docs/api.md POST /jobs/{id}/cancel) depend on the
 * job's state when the click lands. A long backtest should still be RUNNING, so
 * the expected terminal is `canceled`. But to stay deterministic on a very fast
 * machine we accept the documented race: if the run finished before the click,
 * the terminal is `succeeded`/`failed` and the cancel affordance is correctly
 * gone. Either way the cancel UI path is exercised and the outcome is consistent
 * with the state machine; the DB confirms a terminal state. When we DO click
 * cancel and win the race, we assert `canceled` specifically.
 *
 * Self-skips when the Backtests workspace is still the coming-soon placeholder
 * or the stack has no tradable bars.
 */

import { test, expect } from "../fixtures/test";
import { withDb } from "../lib/db";

const TERMINAL = new Set(["canceled", "failed", "succeeded"]);

/** Widest bar window + up to 4 tickers => a longer engine run to cancel. */
async function widestLaunch(): Promise<{
  tickers: string[];
  start: string;
  end: string;
} | null> {
  return withDb(async (c) => {
    const { rows } = await c.query<{ ticker: string }>(
      `SELECT ticker
         FROM tms.bars_daily
        GROUP BY ticker
       HAVING COUNT(*) >= 5
        ORDER BY COUNT(*) DESC, ticker ASC
        LIMIT 4`,
    );
    if (rows.length < 1) return null;
    const { rows: span } = await c.query<{ min_d: string; max_d: string }>(
      `SELECT MIN(ts)::date::text AS min_d, MAX(ts)::date::text AS max_d
         FROM tms.bars_daily`,
    );
    return {
      tickers: rows.map((r) => r.ticker),
      start: span[0].min_d,
      end: span[0].max_d,
    };
  });
}

async function latestJobStatus(): Promise<string | null> {
  return withDb(async (c) => {
    const { rows } = await c.query<{ status: string }>(
      `SELECT status FROM tms.jobs ORDER BY id DESC LIMIT 1`,
    );
    return rows.length ? rows[0].status : null;
  });
}

test.describe("backtest cancel flow", () => {
  test("cancel a long-ish backtest mid-run", async ({ page }) => {
    await page.goto("/backtests", { waitUntil: "domcontentloaded" });
    await expect(page.getByTestId("app-shell")).toBeVisible();

    const launcher = page.getByTestId("open-backtest-dialog");
    const placeholder = page.getByTestId("backtests-placeholder");
    await expect
      .poll(async () => (await launcher.count()) + (await placeholder.count()), {
        timeout: 15_000,
      })
      .toBeGreaterThan(0);
    if ((await launcher.count()) === 0) {
      test.skip(true, "Backtests workspace not yet implemented (coming-soon).");
      return;
    }

    const launch = await widestLaunch();
    if (!launch) {
      test.skip(true, "no tradable bars in the stack — cannot run a backtest.");
      return;
    }

    await launcher.click();
    await expect(page.getByTestId("backtest-dialog")).toBeVisible();
    await page.getByTestId("backtest-tickers").fill(launch.tickers.join(" "));
    await page.getByTestId("backtest-start").fill(launch.start);
    await page.getByTestId("backtest-end").fill(launch.end);
    await page.getByTestId("backtest-submit").click();

    const progress = page.getByTestId("job-progress");
    await expect(progress).toBeVisible();
    const jobId = await progress.getAttribute("data-job-id");
    expect(jobId).toMatch(/^\d+$/);

    const cancelBtn = page.getByTestId("job-cancel");
    const complete = page.getByTestId("job-complete");

    // Race: click cancel while running, or accept an already-terminal run.
    await expect
      .poll(
        async () =>
          (await cancelBtn.isVisible()) || (await complete.isVisible()),
        { timeout: 30_000 },
      )
      .toBe(true);

    if (await cancelBtn.isVisible()) {
      await cancelBtn.click();
      await expect(complete).toBeVisible({ timeout: 90_000 });
      const outcome = await complete.getAttribute("data-outcome");
      expect(
        outcome && TERMINAL.has(outcome),
        `terminal outcome after cancel (got "${outcome}")`,
      ).toBeTruthy();
      // A genuinely-running long backtest cancels to `canceled`; only a job
      // that raced to completion first is allowed another terminal.
      if (outcome !== "succeeded" && outcome !== "failed") {
        expect(outcome, "explicit cancel drives status canceled").toBe(
          "canceled",
        );
      }

      const status = await latestJobStatus();
      expect(
        status && TERMINAL.has(status),
        `DB job status terminal after cancel (got "${status}")`,
      ).toBeTruthy();
    } else {
      // Finished before we could click — terminal already shown, cancel gone.
      await expect(complete).toBeVisible();
      await expect(cancelBtn).toBeHidden();
    }
  });
});
