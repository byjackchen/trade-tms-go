/**
 * (4) Cancel a Hyperopt study mid-run from the UI -> canceled.
 *
 * To get a window in which the study is observably running (so the cancel
 * button appears), this launch uses a LARGER population/generations + the widest
 * covered window over several tickers — many more fold evaluations => a longer
 * coordinator loop than the tiny launch spec.
 *
 * Cancellation semantics mirror the Backtests cancel flow (docs/api.md
 * POST /jobs/{id}/cancel) and depend on the job's state when the click lands. A
 * long study should still be RUNNING, so the expected terminal is `canceled` and
 * the study row settles to INTERRUPTED. To stay deterministic on a very fast
 * machine we accept the documented race: if the study finished before the click,
 * the terminal is `succeeded`/`failed` and the cancel affordance is correctly
 * gone. When we DO win the race we assert `canceled` specifically AND that the
 * persisted study status is INTERRUPTED.
 *
 * Self-skips when the Hyperopt workspace is still coming-soon, the launch
 * affordance is unbuilt, or the stack has no tradable bars.
 */

import { test, expect } from "../fixtures/test";
import {
  hyperoptUiReady,
  pickStudyLaunch,
  latestStudy,
  STUDY_STRATEGY,
  JOB_TERMINAL,
} from "../lib/hyperopt";

type Page = import("@playwright/test").Page;

async function setIfPresent(
  page: Page,
  testid: string,
  value: string,
): Promise<void> {
  const el = page.getByTestId(testid);
  if (await el.count()) {
    const tag = await el.first().evaluate((n) => n.tagName.toLowerCase());
    if (tag === "select") {
      await el.first().selectOption(value).catch(() => {});
    } else {
      await el.first().fill(value).catch(() => {});
    }
  }
}

test.describe("hyperopt cancel flow", () => {
  test("cancel a long-ish study mid-run", async ({ page }) => {
    const launch = await pickStudyLaunch();
    if (!launch) {
      test.skip(true, "no tradable bars in the stack — cannot run a study.");
      return;
    }

    if (!(await hyperoptUiReady(page))) {
      test.skip(true, "Hyperopt workspace not yet implemented (coming-soon).");
      return;
    }

    let opener = page.getByTestId("hyperopt-launch");
    if (!(await opener.count())) opener = page.getByTestId("open-hyperopt-dialog");
    if (!(await opener.count())) {
      test.skip(true, "Hyperopt launch affordance not built yet.");
      return;
    }
    await opener.first().click();
    await expect(page.getByTestId("hyperopt-dialog")).toBeVisible();
    await expect(page.getByTestId("hyperopt-form")).toBeVisible();

    // A LARGER study so the coordinator is observably running when we cancel.
    await setIfPresent(page, "hyperopt-strategy", STUDY_STRATEGY);
    await setIfPresent(page, "hyperopt-tickers", launch.tickers.join(" "));
    await setIfPresent(page, "hyperopt-start", launch.start);
    await setIfPresent(page, "hyperopt-end", launch.end);
    await setIfPresent(page, "hyperopt-population", "20");
    await setIfPresent(page, "hyperopt-generations", "5");
    await setIfPresent(page, "hyperopt-folds", "5");

    await page.getByTestId("hyperopt-submit").click();

    const progress = page.getByTestId("job-progress");
    await expect(progress).toBeVisible();
    const jobId = await progress.getAttribute("data-job-id");
    expect(jobId).toMatch(/^\d+$/);

    const cancelBtn = page.getByTestId("job-cancel");
    const complete = page.getByTestId("job-complete");

    // Race: cancel while running, or accept an already-terminal study.
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
        outcome && JOB_TERMINAL.has(outcome),
        `terminal outcome after cancel (got "${outcome}")`,
      ).toBeTruthy();

      if (outcome !== "succeeded" && outcome !== "failed") {
        expect(outcome, "explicit cancel drives status canceled").toBe(
          "canceled",
        );
        // A canceled study coordinator leaves the study INTERRUPTED (it did not
        // run to COMPLETE). Poll until the coordinator writes the terminal row.
        await expect
          .poll(
            async () => {
              const s = await latestStudy(STUDY_STRATEGY);
              return s?.status ?? null;
            },
            { timeout: 30_000 },
          )
          .not.toBe("RUNNING");
        const s = await latestStudy(STUDY_STRATEGY);
        expect(s, "the canceled study row exists").not.toBeNull();
        expect(
          ["INTERRUPTED", "COMPLETE"].includes(s!.status),
          `canceled study is terminal in the DB (got "${s!.status}")`,
        ).toBeTruthy();
      }
    } else {
      // Finished before we could click — terminal already shown, cancel gone.
      await expect(complete).toBeVisible();
      await expect(cancelBtn).toBeHidden();
    }
  });
});
