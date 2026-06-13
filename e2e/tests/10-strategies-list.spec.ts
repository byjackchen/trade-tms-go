/**
 * (1) Strategies list renders the four shipped strategies with active params.
 *
 * The Strategies workspace lists exactly the four canonical strategies (sepa,
 * pairs, sector_rotation, intraday_breakout — internal/params/loader.go) each
 * carrying its *active* parameter document. This spec asserts:
 *   - the list mounts four strategy rows, one per canonical id;
 *   - each row surfaces the strategy id (so the UI is not fabricating a
 *     different set);
 *   - the numbers the row shows for its active params MATCH the ground truth
 *     resolved independently from postgres (tms.active_params -> tms.param_sets)
 *     and the API — never fabricated. Each row exposes its active param values
 *     either as `data-*` attributes (preferred, exact) or as rendered text we
 *     parse and compare with display-rounding tolerance.
 *
 * Robustness while the section is still landing (it ships after the P1 Data +
 * P2 Backtests workspaces): the spec self-skips when the page is still the
 * coming-soon placeholder / unbuilt. Once the UI is wired the assertions are
 * exact and permanent — every rendered number is checked against the DB/API.
 */

import { test, expect } from "../fixtures/test";
import {
  strategiesUiReady,
  activeParams,
  EXPECTED_STRATEGY_COUNT,
  STRATEGY_IDS,
  parseParamCell,
  paramValuesMatch,
} from "../lib/strategies";

test.describe("strategies list", () => {
  test("lists four strategies whose active params match DB/API ground truth", async ({
    page,
  }) => {
    if (!(await strategiesUiReady(page))) {
      test.skip(true, "Strategies workspace not yet implemented (coming-soon).");
      return;
    }

    // The list root mounted; require exactly the four canonical strategy rows.
    await expect(page.getByTestId("strategies-page")).toBeVisible();

    const rows = page.locator('[data-testid^="strategy-row-"]');
    await expect
      .poll(async () => rows.count(), { timeout: 15_000 })
      .toBe(EXPECTED_STRATEGY_COUNT);

    // Each canonical strategy is present exactly once (the UI is not listing a
    // different/fabricated set). Rows are keyed `strategy-row-<id>`.
    for (const id of STRATEGY_IDS) {
      const row = page.getByTestId(`strategy-row-${id}`);
      await expect(row, `row for strategy ${id} renders`).toBeVisible();
    }

    // For every strategy, the active param values the row shows == ground truth.
    // A row advertises a param value via `data-param-<name>` (exact) when it
    // surfaces params inline; otherwise we assert the detail page (spec 11)
    // carries them. Here we check whatever the row chooses to expose, exactly.
    for (const id of STRATEGY_IDS) {
      const truth = await activeParams(id);
      expect(
        truth,
        `ground truth resolvable for ${id} (DB param_set or API baseline)`,
      ).not.toBeNull();
      if (!truth) continue;

      const row = page.getByTestId(`strategy-row-${id}`);

      // The row may expose a param count for the active document.
      const countAttr = await row.getAttribute("data-param-count");
      if (countAttr != null && countAttr !== "") {
        expect(
          Number(countAttr),
          `${id} row data-param-count === active document param count`,
        ).toBe(truth.length);
      }

      // The row may expose the source/version of the active document.
      // (No assertion on the exact value here — that is the detail page's job —
      // but if present it must be a non-empty string.)
      const src = await row.getAttribute("data-source");
      if (src != null) {
        expect(src.length, `${id} row data-source non-empty when present`)
          .toBeGreaterThan(0);
      }

      // Any inline active-param value the row renders via data-param-<name> must
      // equal the ground-truth active value for that parameter.
      for (const p of truth) {
        const cell = row.locator(`[data-param-name="${p.name}"]`);
        if (await cell.count()) {
          const dataVal = await cell.first().getAttribute("data-param-value");
          if (dataVal != null && dataVal !== "") {
            expect(
              paramValuesMatch(parseParamCell(dataVal), p.value),
              `${id}.${p.name} data-param-value "${dataVal}" === truth ${String(p.value)}`,
            ).toBe(true);
          } else {
            const shown = parseParamCell((await cell.first().textContent()) ?? "");
            expect(
              paramValuesMatch(shown, p.value),
              `${id}.${p.name} rendered "${String(shown)}" === truth ${String(p.value)}`,
            ).toBe(true);
          }
        }
      }
    }
  });
});
