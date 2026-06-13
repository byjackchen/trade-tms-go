/**
 * (3) Hyperopt promotion flow, end to end.
 *
 * From the detail page of a study with a COMPLETE trial:
 *   1. click the promote affordance for a chosen COMPLETE trial;
 *   2. confirm the promotion (the one-click-with-confirmation gate that replaces
 *      the Python reference's git-review — docs/api.md POST /hyperopt/{id}/promote,
 *      locked decision 6);
 *   3. observe the success state;
 *   4. assert tms.active_params for the trial's strategy now points at the
 *      promoted param_set AND carries a full audit row (promoted_by /
 *      source_study / source_trial); the active param_set's source is `tuned`
 *      and source_id is `hyperopt:<study_ts>`;
 *   5. navigate to /strategies/{strategy} and confirm the detail page renders
 *      the NEW active params — i.e. the strategies workspace reflects the
 *      promotion (the run path resolves active_params -> param_sets.payload).
 *
 * Ground truth comes from postgres (tms.active_params JOIN tms.param_sets) and
 * the API; the trial's params are the promoted document. No fabricated values.
 *
 * Self-skips when there is no study with a COMPLETE, promotable trial yet, or
 * the detail / promotion UI is not built.
 */

import { test, expect } from "../fixtures/test";
import {
  hyperoptDetailReady,
  latestStudy,
  studyTrials,
  firstCompleteTrial,
  activePromotion,
  promotionAuditCount,
  studyByTs,
} from "../lib/hyperopt";
import {
  strategyDetailReady,
  activeParams,
  parseParamCell,
  paramValuesMatch,
} from "../lib/strategies";

const PROMOTABLE_STRATEGIES = new Set([
  "sepa",
  "sector_rotation",
  "pairs",
]);

test.describe("hyperopt promotion flow", () => {
  test("promote a trial -> active_params + audit updated, /strategies reflects it", async ({
    page,
  }) => {
    const study = await latestStudy();
    if (!study) {
      test.skip(true, "no hyperopt study persisted yet — nothing to promote.");
      return;
    }
    // `joint` promotes every sub-strategy; this spec asserts a single-strategy
    // promotion's effect precisely, so target a single-strategy study.
    if (!PROMOTABLE_STRATEGIES.has(study.strategy)) {
      test.skip(
        true,
        `newest study strategy "${study.strategy}" is not a single promotable strategy.`,
      );
      return;
    }

    const trials = await studyTrials(study.studyTs);
    const target = firstCompleteTrial(trials);
    if (!target) {
      test.skip(true, "study has no COMPLETE (promotable) trial.");
      return;
    }
    // A promotable trial must carry tunable params (the §422 rule).
    if (Object.keys(target.params).length === 0) {
      test.skip(true, "COMPLETE trial has no tunable params to promote.");
      return;
    }

    if (!(await hyperoptDetailReady(page, study.studyTs))) {
      test.skip(true, "Hyperopt detail page not yet implemented (coming-soon).");
      return;
    }
    await expect(page.getByTestId("hyperopt-detail")).toBeVisible();

    // Locate the promote affordance for the target trial.
    let promote = page.getByTestId(`hyperopt-promote-${target.number}`);
    if (!(await promote.count())) {
      // The promote control may live inside the trial row; reveal it by hovering
      // / selecting the row first.
      const row = page.getByTestId(`hyperopt-trial-row-${target.number}`);
      if (await row.count()) {
        await row.click().catch(() => {});
      }
      promote = page.getByTestId(`hyperopt-promote-${target.number}`);
    }
    if (!(await promote.count())) {
      test.skip(true, "Promotion affordance not built yet.");
      return;
    }

    await promote.first().click();

    // Confirmation gate (replaces the Python git-review). Accept it.
    const confirm = page.getByTestId("hyperopt-promote-confirm");
    await expect(confirm, "promotion confirmation appears").toBeVisible();
    await confirm.click();

    // Success state.
    await expect(
      page.getByTestId("hyperopt-promote-success"),
      "promotion success state shows",
    ).toBeVisible({ timeout: 30_000 });

    // ----- DB: active_params now points at the promoted trial, with audit -----
    await expect
      .poll(async () => (await activePromotion(study.strategy)) !== null, {
        timeout: 30_000,
      })
      .toBe(true);
    const promo = await activePromotion(study.strategy);
    expect(promo, "active_params row exists for the strategy").not.toBeNull();

    // The audit trail records THIS study + trial.
    expect(promo!.sourceStudy, "audit source_study === the study").toBe(
      study.studyTs,
    );
    expect(promo!.sourceTrial, "audit source_trial === the promoted trial").toBe(
      target.number,
    );
    expect(promo!.promotedBy, "audit promoted_by is recorded").toBeTruthy();
    expect(promo!.promotedBy.length, "promoted_by is non-empty").toBeGreaterThan(
      0,
    );
    // The promoted param_set is a tuned snapshot tagged with the study.
    expect(promo!.source, "promoted param_set source === tuned").toBe("tuned");
    expect(
      promo!.sourceId,
      "active_params source_id === hyperopt:<study_ts>",
    ).toBe(`hyperopt:${study.studyTs}`);

    // An audit row keyed to (study, trial) exists (independent count check).
    const auditN = await promotionAuditCount(study.studyTs, target.number);
    expect(auditN, "an audit row for (study, trial) exists").toBeGreaterThan(0);

    // The study itself is unaffected by promotion (read-back sanity).
    const studyAfter = await studyByTs(study.studyTs);
    expect(studyAfter, "study still present after promotion").not.toBeNull();

    // ----- /strategies/{strategy} now reflects the promoted active params -----
    const truth = await activeParams(study.strategy);
    expect(
      truth,
      "active params now resolvable for the promoted strategy",
    ).not.toBeNull();

    if (!(await strategyDetailReady(page, study.strategy))) {
      test.skip(
        true,
        "Strategies detail page not yet implemented — cannot verify reflection.",
      );
      return;
    }
    await expect(page.getByTestId("strategy-detail")).toBeVisible();

    // The strategy detail's active-param table must render the promoted values.
    const paramRows = page.locator('[data-testid^="param-row-"]');
    await expect
      .poll(async () => paramRows.count(), { timeout: 15_000 })
      .toBe(truth!.length);

    for (const p of truth!) {
      const row = page.getByTestId(`param-row-${p.name}`);
      await expect(
        row,
        `promoted param row ${study.strategy}.${p.name} renders`,
      ).toBeVisible();
      const dataVal = await row.getAttribute("data-param-value");
      if (dataVal != null && dataVal !== "") {
        expect(
          paramValuesMatch(parseParamCell(dataVal), p.value),
          `${p.name} data-param-value "${dataVal}" === promoted truth ${String(p.value)}`,
        ).toBe(true);
        continue;
      }
      const valueCell = page.getByTestId(`param-value-${p.name}`);
      const text = (await valueCell.count())
        ? (await valueCell.first().textContent()) ?? ""
        : (await row.textContent()) ?? "";
      expect(
        paramValuesMatch(parseParamCell(text), p.value),
        `${p.name} rendered "${text}" === promoted truth ${String(p.value)}`,
      ).toBe(true);
    }
  });
});
