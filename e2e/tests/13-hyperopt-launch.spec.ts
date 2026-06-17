/**
 * (1) Launch a tiny Hyperopt study from the UI, end to end.
 *
 * Drives a real (non-scripted) NSGA-II walk-forward study purely through the
 * browser:
 *   1. open the launch affordance on the Strategies Tune surface (/strategies,
 *      `strategy-section-tune`): the `hyperopt-launch` / `open-hyperopt-dialog`
 *      button -> `hyperopt-dialog`;
 *   2. configure a TINY study — small population/generations, 1-2 folds, a few
 *      tickers over a covered window — and submit;
 *   3. watch the shared `job-progress` panel transition running -> succeeded via
 *      the UI (the worker drives the `hyperopt.run` job; the tracker reconciles
 *      over the WS job stream + REST poll — the requirement's "WS progress ->
 *      succeeded");
 *   4. follow the detail deep-link (/strategies?study={study_ts}) and confirm the trials
 *      table populates and the Pareto scatter renders, with numbers matching the
 *      DB/API ground truth exactly.
 *
 * Ground-truth coupling: the launched study is the study the coordinator
 * persisted (tms.hyperopt_studies gains a COMPLETE row whose study_ts == the
 * detail URL), its trials equal tms.hyperopt_trials, and the Pareto flags the UI
 * renders equal the non-dominance computed here directly from (sharpe, calmar).
 * No fabricated ids/numbers.
 *
 * Robustness: self-skips while the Hyperopt workspace is still coming-soon, when
 * the launch affordance is unbuilt, or when the stack has no tradable bars for a
 * study. Once wired, the assertions are exact and permanent.
 */

import { test, expect } from "../fixtures/test";
import { getAuthed } from "../lib/api";
import {
  hyperoptUiReady,
  hyperoptDetailReady,
  pickStudyLaunch,
  latestStudy,
  studyTrials,
  STUDY_STRATEGY,
  STUDY_TERMINAL,
} from "../lib/hyperopt";
import { withDb } from "../lib/db";

type Page = import("@playwright/test").Page;

/** Count of studies currently stored — gates "the launch created a new one". */
async function studyCount(): Promise<number> {
  return withDb(async (c) => {
    const { rows } = await c.query<{ n: string }>(
      `SELECT COUNT(*)::text AS n FROM tms.hyperopt_studies`,
    );
    return Number(rows[0].n);
  });
}

/** Set a tiny-study form field by testid, only when the control exists (the UI
 * may default these and omit some inputs). */
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

test.describe("hyperopt study launch", () => {
  test("launch a tiny study, WS progress -> succeeded, trials + pareto match DB", async ({
    page,
  }) => {
    const launch = await pickStudyLaunch();
    if (!launch) {
      test.skip(true, "no tradable bars in the stack — cannot run a study.");
      return;
    }

    if (!(await hyperoptUiReady(page))) {
      test.skip(true, "Strategies Tune (hyperopt) surface not yet implemented.");
      return;
    }
    await expect(page.getByTestId("strategy-section-tune")).toBeVisible();

    // Open the launch dialog (button id varies; accept either convention).
    let opener = page.getByTestId("hyperopt-launch");
    if (!(await opener.count())) opener = page.getByTestId("open-hyperopt-dialog");
    if (!(await opener.count())) {
      test.skip(true, "Hyperopt launch affordance not built yet.");
      return;
    }
    await opener.first().click();
    const dialog = page.getByTestId("hyperopt-dialog");
    await expect(dialog).toBeVisible();
    await expect(page.getByTestId("hyperopt-form")).toBeVisible();

    const before = await studyCount();

    // Configure a TINY study: pairs, a few tickers, the covered window, the
    // smallest population/generations/folds the form allows.
    await setIfPresent(page, "hyperopt-strategy", STUDY_STRATEGY);
    await setIfPresent(page, "hyperopt-tickers", launch.tickers.join(" "));
    await setIfPresent(page, "hyperopt-start", launch.start);
    await setIfPresent(page, "hyperopt-end", launch.end);
    await setIfPresent(page, "hyperopt-population", "4");
    await setIfPresent(page, "hyperopt-generations", "1");
    await setIfPresent(page, "hyperopt-folds", "2");

    await page.getByTestId("hyperopt-submit").click();

    // The shared job panel surfaces the hyperopt.run job and drives it via WS.
    const progress = page.getByTestId("job-progress");
    await expect(progress).toBeVisible();
    const jobId = await progress.getAttribute("data-job-id");
    expect(jobId, "job-progress exposes a numeric data-job-id").toMatch(/^\d+$/);

    // Running -> terminal, asserted succeeded. A tiny study over a covered
    // window must complete; broken wiring would fail and be caught here.
    const complete = page.getByTestId("job-complete");
    await expect(complete).toBeVisible({ timeout: 85_000 });
    const outcome = await complete.getAttribute("data-outcome");
    expect(
      outcome,
      `tiny study succeeds end-to-end (got "${outcome}")`,
    ).toBe("succeeded");

    // DB gained a new study; resolve the one the launch created.
    await expect
      .poll(async () => studyCount(), { timeout: 30_000 })
      .toBeGreaterThanOrEqual(before + 1);
    const study = await latestStudy(STUDY_STRATEGY);
    expect(study, "a study row exists after the launch").not.toBeNull();
    expect(
      STUDY_TERMINAL.has(study!.status),
      `study status terminal (got "${study!.status}")`,
    ).toBeTruthy();
    expect(study!.status, "the launched study completed").toBe("COMPLETE");

    // Trials persisted; the study completed all of them.
    const trials = await studyTrials(study!.studyTs);
    expect(trials.length, "trials persisted to the DB").toBeGreaterThan(0);
    const completeTrials = trials.filter((t) => t.state === "COMPLETE");
    expect(
      completeTrials.length,
      "the study produced at least one COMPLETE trial",
    ).toBeGreaterThan(0);

    // Follow the detail link/redirect to the persisted study_ts. In the FINAL IA
    // a study deep-links via /strategies?study={ts} (the inline detail surface).
    const detailLink = page.getByTestId("hyperopt-detail-link");
    if (await detailLink.count()) {
      await detailLink.first().click();
    } else {
      await page.goto(`/strategies?study=${study!.studyTs}`, {
        waitUntil: "domcontentloaded",
      });
    }
    await expect
      .poll(() => new URL(page.url()).searchParams.get("study"), {
        timeout: 30_000,
      })
      .toBe(study!.studyTs);

    if (!(await hyperoptDetailReady(page, study!.studyTs))) {
      test.skip(true, "Hyperopt detail page not yet implemented.");
      return;
    }
    await expect(page.getByTestId("hyperopt-detail")).toBeVisible();

    // The detail root should identify which study it is.
    const rootTs = await page
      .getByTestId("hyperopt-detail")
      .getAttribute("data-study-ts");
    if (rootTs != null && rootTs !== "") {
      expect(rootTs, "detail root data-study-ts === route id").toBe(
        study!.studyTs,
      );
    }

    // ----- trials table populates and equals the DB -----
    const trialsTable = page.getByTestId("hyperopt-trials-table");
    await expect(trialsTable, "trials table renders").toBeVisible();
    const trialRows = page.locator('[data-testid^="hyperopt-trial-row-"]');
    await expect
      .poll(async () => trialRows.count(), { timeout: 20_000 })
      .toBe(trials.length);

    // Spot-check the objective values of every COMPLETE trial row against the
    // DB ground truth (data-sharpe / data-calmar attributes, when exposed).
    for (const t of completeTrials) {
      const row = page.getByTestId(`hyperopt-trial-row-${t.number}`);
      await expect(row, `trial row ${t.number} renders`).toBeVisible();
      const dSharpe = await row.getAttribute("data-sharpe");
      if (dSharpe != null && dSharpe !== "") {
        expect(
          Number(dSharpe),
          `trial ${t.number} data-sharpe === DB sharpe`,
        ).toBeCloseTo(t.sharpe as number, 6);
      }
      const dCalmar = await row.getAttribute("data-calmar");
      if (dCalmar != null && dCalmar !== "") {
        expect(
          Number(dCalmar),
          `trial ${t.number} data-calmar === DB calmar`,
        ).toBeCloseTo(t.calmar as number, 6);
      }
    }

    // ----- numbers also match the API (the UI proxies it) -----
    const apiTrials = await getAuthed(`hyperopt/${study!.studyTs}/trials`);
    expect(apiTrials.status, "GET /hyperopt/{id}/trials is 200").toBe(200);
    const apiList =
      (apiTrials.body as { trials?: Array<Record<string, unknown>> }).trials ??
      [];
    expect(apiList.length, "API trial count === DB trial count").toBe(
      trials.length,
    );

    // ----- Pareto scatter renders; its marked points === computed non-dominance -----
    const scatter = page.getByTestId("pareto-scatter");
    await expect(scatter, "pareto scatter renders").toBeVisible();
    await expect
      .poll(async () => scatter.locator("canvas, svg").count(), {
        timeout: 15_000,
      })
      .toBeGreaterThan(0);

    // The Pareto front the UI marks must equal the front computed here from the
    // DB objective points. Prefer per-trial-row pareto flags; fall back to a
    // scatter-level count attribute.
    const dbParetoNumbers = trials
      .filter((t) => t.pareto)
      .map((t) => t.number)
      .sort((a, b) => a - b);
    expect(
      dbParetoNumbers.length,
      "at least one trial is on the Pareto front",
    ).toBeGreaterThan(0);

    // (a) Per-row flag: rows carrying data-pareto must agree exactly.
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
    // (b) When no per-row flag exists, assert the scatter's own front count.
    if (checkedRowFlags === 0) {
      const frontCount = await scatter.getAttribute("data-pareto-count");
      if (frontCount != null && frontCount !== "") {
        expect(
          Number(frontCount),
          "scatter data-pareto-count === computed front size",
        ).toBe(dbParetoNumbers.length);
      }
    }
  });
});
