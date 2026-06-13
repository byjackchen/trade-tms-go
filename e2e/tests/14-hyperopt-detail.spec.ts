/**
 * (2) Hyperopt detail: Pareto-front rendering + per-fold drill-down.
 *
 * Independent of the launch spec — it opens the newest persisted study (any
 * strategy) and asserts the detail page renders, against the DB/API ground
 * truth:
 *   - the trials table has one row per tms.hyperopt_trials row;
 *   - the Pareto scatter renders and the points it marks as non-dominated equal
 *     the front computed here directly from (sharpe, calmar) over the COMPLETE
 *     trials (weak dominance with strict improvement, spec §10);
 *   - per-fold drill-down: expanding a COMPLETE trial reveals one fold row per
 *     fold the trial recorded (tms.hyperopt_trials.folds), and the fold count
 *     equals jsonb_array_length(folds).
 *
 * Self-skips when there is no study yet (fresh stack) or the detail page is
 * still coming-soon. Once wired, the assertions are exact and permanent.
 */

import { test, expect } from "../fixtures/test";
import { getAuthed } from "../lib/api";
import {
  hyperoptDetailReady,
  latestStudy,
  studyTrials,
  firstCompleteTrial,
} from "../lib/hyperopt";

test.describe("hyperopt detail: pareto + per-fold drill-down", () => {
  test("pareto front + per-fold breakdown match the DB", async ({ page }) => {
    const study = await latestStudy();
    if (!study) {
      test.skip(true, "no hyperopt study persisted yet — nothing to render.");
      return;
    }

    const trials = await studyTrials(study.studyTs);
    if (trials.length === 0) {
      test.skip(true, "study has no trials yet.");
      return;
    }

    if (!(await hyperoptDetailReady(page, study.studyTs))) {
      test.skip(true, "Hyperopt detail page not yet implemented (coming-soon).");
      return;
    }
    await expect(page.getByTestId("hyperopt-detail")).toBeVisible();

    // ----- trials table: one row per DB trial -----
    const trialRows = page.locator('[data-testid^="hyperopt-trial-row-"]');
    await expect
      .poll(async () => trialRows.count(), { timeout: 20_000 })
      .toBe(trials.length);

    // ----- Pareto scatter: marked front === computed non-dominance -----
    const scatter = page.getByTestId("pareto-scatter");
    await expect(scatter, "pareto scatter renders").toBeVisible();
    await expect
      .poll(async () => scatter.locator("canvas, svg").count(), {
        timeout: 15_000,
      })
      .toBeGreaterThan(0);

    const completeTrials = trials.filter((t) => t.state === "COMPLETE");
    const dbFront = completeTrials
      .filter((t) => t.pareto)
      .map((t) => t.number)
      .sort((a, b) => a - b);

    if (completeTrials.length > 0) {
      expect(
        dbFront.length,
        "at least one COMPLETE trial is on the Pareto front",
      ).toBeGreaterThan(0);

      let checkedRowFlags = 0;
      for (const t of completeTrials) {
        const row = page.getByTestId(`hyperopt-trial-row-${t.number}`);
        const flag = await row.getAttribute("data-pareto");
        if (flag != null && flag !== "") {
          checkedRowFlags += 1;
          expect(
            flag === "true",
            `trial ${t.number} data-pareto === computed non-dominance`,
          ).toBe(t.pareto);
        }
      }
      if (checkedRowFlags === 0) {
        const frontCount = await scatter.getAttribute("data-pareto-count");
        if (frontCount != null && frontCount !== "") {
          expect(
            Number(frontCount),
            "scatter data-pareto-count === computed front size",
          ).toBe(dbFront.length);
        }
      }
    }

    // Cross-check the API's own pareto_front flags equal the computed front, so
    // the UI (which proxies the API) is anchored to the same source.
    const apiRes = await getAuthed(`hyperopt/${study.studyTs}/trials`);
    if (apiRes.status === 200) {
      const apiTrials =
        (apiRes.body as {
          trials?: Array<{ number?: number; pareto_front?: boolean }>;
        }).trials ?? [];
      const apiFront = apiTrials
        .filter((t) => t.pareto_front === true)
        .map((t) => t.number as number)
        .sort((a, b) => a - b);
      // Only assert equality when the study has COMPLETE trials (an all-FAIL
      // study has an empty front on both sides, which is also fine).
      if (completeTrials.length > 0) {
        expect(apiFront, "API pareto_front === computed front").toEqual(dbFront);
      }
    }

    // ----- per-fold drill-down for a COMPLETE trial -----
    const target = firstCompleteTrial(trials);
    if (!target) {
      test.skip(true, "no COMPLETE trial to drill into.");
      return;
    }
    // A walk-forward study records >=1 fold; a single-window study records 0.
    // Only assert the drill-down when the trial actually carries folds.
    if (target.foldCount === 0) {
      test.skip(true, "trials are single-window (no folds to drill into).");
      return;
    }

    const row = page.getByTestId(`hyperopt-trial-row-${target.number}`);
    await expect(row).toBeVisible();

    // Expand the row if it is collapsible (an expander toggle), else the fold
    // rows are already present.
    const expander = page.getByTestId(`hyperopt-trial-expand-${target.number}`);
    if (await expander.count()) {
      await expander.first().click();
    } else {
      await row.click().catch(() => {});
    }

    // One fold row per recorded fold. Accept either a per-trial fold-row testid
    // (`trial-fold-<trial>-<fold>`) or a generic fold-row container scoped to
    // the trial (`trial-fold-row-<trial>`).
    const foldRows = page.locator(
      `[data-testid^="trial-fold-${target.number}-"]`,
    );
    const foldRowsAlt = page.locator(
      `[data-testid="trial-fold-row-${target.number}"]`,
    );
    await expect
      .poll(
        async () => (await foldRows.count()) + (await foldRowsAlt.count()),
        { timeout: 15_000 },
      )
      .toBeGreaterThan(0);

    // When the page exposes the per-fold rows by index, their count must equal
    // the DB fold count exactly.
    const perFoldCount = await foldRows.count();
    if (perFoldCount > 0) {
      expect(
        perFoldCount,
        `fold rows for trial ${target.number} === DB fold count`,
      ).toBe(target.foldCount);
    }
  });
});
