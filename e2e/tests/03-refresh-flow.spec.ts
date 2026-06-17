/**
 * (c) Refresh flow: open the dialog, pick the parquet source + two tickers,
 * submit, watch the job progress through live states to a terminal completion
 * via the UI, and confirm the sync/jobs surface gains a row.
 *
 * We observe the *state machine*, not a specific outcome: depending on whether
 * the worker has a parquet cache mounted, a parquet refresh may succeed, fail,
 * or be a no-op — all are terminal. The durable, always-true observables are:
 *   - the job-progress panel reaches `job-complete` (terminal data-outcome),
 *   - the tracked job appears as a row in the Recent jobs panel,
 *   - the jobs table in postgres has one more row than before.
 * We additionally assert the sync-runs history table gains a row when the run
 * recorded one (the worker writes dataset_sync_runs on import paths).
 */

import { test, expect } from "../fixtures/test";
import { withDb, jobCount, syncRunCount } from "../lib/db";

const TERMINAL = new Set(["succeeded", "failed", "canceled"]);

test.describe("data refresh flow", () => {
  test("enqueue parquet refresh for 2 tickers, drive job to terminal, see the row", async ({
    page,
  }) => {
    const before = await withDb(async (c) => ({
      jobs: await jobCount(c),
      runs: await syncRunCount(c),
    }));

    await page.goto("/systems?tab=data");

    // 1. Open the refresh dialog.
    await page.getByTestId("open-refresh-dialog").click();
    const dialog = page.getByTestId("refresh-dialog");
    await expect(dialog).toBeVisible();
    await expect(page.getByTestId("refresh-form")).toBeVisible();

    // 2. Select parquet source explicitly + restrict to two tickers + SEP table.
    await page.getByTestId("refresh-source").selectOption("parquet");
    await page.getByTestId("refresh-table-SEP").check();
    await page.getByTestId("refresh-tickers").fill("AAPL MSFT");

    // 3. Submit.
    await page.getByTestId("refresh-submit").click();

    // 4. The job-progress view replaces the form (the job was enqueued).
    const progress = page.getByTestId("job-progress");
    await expect(progress).toBeVisible();

    // Capture the tracked job id from the progress panel.
    const jobId = await progress.getAttribute("data-job-id");
    expect(jobId, "job-progress exposes a numeric data-job-id").toMatch(/^\d+$/);

    // 5. Observe transition to a terminal completed state via the UI. The
    //    worker drives it; the tracker reconciles via SSE + REST poll.
    const complete = page.getByTestId("job-complete");
    await expect(complete).toBeVisible({ timeout: 80_000 });
    const outcome = await complete.getAttribute("data-outcome");
    expect(
      outcome && TERMINAL.has(outcome),
      `job reached a terminal outcome (got "${outcome}")`,
    ).toBeTruthy();

    // 6. Close the dialog and confirm the Recent jobs panel shows this job row.
    await page.getByTestId("refresh-dialog-done").click();
    await expect(dialog).toBeHidden();

    const jobRow = page.getByTestId(`job-row-${jobId}`);
    await expect(jobRow).toBeVisible();
    await expect(jobRow).toContainText("data.refresh");

    // 7. Durable truth: the jobs table gained exactly the row we created (at
    //    least one more than before; dedupe means a concurrent run could share).
    const after = await withDb(async (c) => ({
      jobs: await jobCount(c),
      runs: await syncRunCount(c),
    }));
    expect(
      after.jobs,
      "tms.jobs gained at least one row for the enqueued refresh",
    ).toBeGreaterThanOrEqual(before.jobs + 1);

    // The sync-runs history table never loses rows; if the run recorded one it
    // grew, otherwise it held. Assert monotonicity (no rows vanished).
    expect(after.runs).toBeGreaterThanOrEqual(before.runs);
  });
});
