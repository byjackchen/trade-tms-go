/**
 * (2) Strategy detail param table matches active_params in the DB.
 *
 * For each canonical strategy, /strategies/{id} renders a param table of the
 * strategy's *active* parameters. This spec asserts the table contains a row per
 * active parameter whose value MATCHES the ground truth resolved independently
 * from postgres (tms.active_params -> tms.param_sets.payload.parameters[*].default)
 * and the API — never fabricated.
 *
 * Contract (conventional `data-testid`s mirroring the Backtests detail page):
 *   - `strategy-detail`               detail root (presence => real page)
 *   - `param-row-<name>`              one row per active parameter
 *   - each row exposes the value via `data-param-value` (exact) or rendered text
 *   - the table's row count === the active document's parameter count
 *
 * Self-skips per strategy while the section is still the coming-soon placeholder
 * or the route is unbuilt; once wired, the assertions are exact and permanent.
 */

import { test, expect } from "../fixtures/test";
import {
  strategyDetailReady,
  activeParams,
  STRATEGY_IDS,
  parseParamCell,
  paramValuesMatch,
} from "../lib/strategies";

test.describe("strategy detail param table", () => {
  for (const id of STRATEGY_IDS) {
    test(`${id} detail param table === active_params in DB`, async ({ page }) => {
      const truth = await activeParams(id);
      if (!truth) {
        test.skip(true, `no resolvable active params for ${id} (DB/API empty).`);
        return;
      }

      if (!(await strategyDetailReady(page, id))) {
        test.skip(
          true,
          "Strategies detail page not yet implemented (coming-soon).",
        );
        return;
      }

      await expect(page.getByTestId("strategy-detail")).toBeVisible();

      // The detail page should identify which strategy it is (guards against a
      // wrong-id render). When the root carries data-strategy, it must match.
      const rootId = await page
        .getByTestId("strategy-detail")
        .getAttribute("data-strategy");
      if (rootId != null && rootId !== "") {
        expect(rootId, "detail root data-strategy === route id").toBe(id);
      }

      // One param row per active parameter, value-matched to ground truth.
      const paramRows = page.locator('[data-testid^="param-row-"]');
      await expect
        .poll(async () => paramRows.count(), { timeout: 15_000 })
        .toBe(truth.length);

      for (const p of truth) {
        const row = page.getByTestId(`param-row-${p.name}`);
        await expect(row, `param row for ${id}.${p.name} renders`).toBeVisible();

        // Prefer the exact data-param-value; fall back to parsing the value cell.
        const dataVal = await row.getAttribute("data-param-value");
        if (dataVal != null && dataVal !== "") {
          expect(
            paramValuesMatch(parseParamCell(dataVal), p.value),
            `${id}.${p.name} data-param-value "${dataVal}" === truth ${String(p.value)}`,
          ).toBe(true);
          continue;
        }

        // The value lives in a dedicated cell, or in the row text. Prefer a
        // `param-value-<name>` cell when present for an unambiguous read.
        const valueCell = page.getByTestId(`param-value-${p.name}`);
        const text =
          (await valueCell.count())
            ? (await valueCell.first().textContent()) ?? ""
            : (await row.textContent()) ?? "";
        const shown = parseParamCell(text);
        expect(
          paramValuesMatch(shown, p.value),
          `${id}.${p.name} rendered "${String(shown)}" === truth ${String(p.value)}`,
        ).toBe(true);
      }
    });
  }
});
