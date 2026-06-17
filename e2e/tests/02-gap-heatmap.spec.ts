/**
 * (b) The gap heatmap renders.
 *
 * Two paths are exercised:
 *   1. Typing a ticker into the heatmap's own input and inspecting it.
 *   2. The coverage table's "Gaps" inspect button wiring the ticker into the
 *      heatmap (the cross-component flow on the Data page).
 *
 * The seed plants GAPPY (missing interior sessions) and CLEAN (complete), so we
 * can assert both the "missing" and "complete" rendering branches deterministically
 * when those tickers are present. When the stack carries real data instead, the
 * test falls back to the worst-gap ticker the coverage table surfaces, so it is
 * meaningful with or without the seed.
 */

import { test, expect } from "../fixtures/test";
import { withDb, tableTruth } from "../lib/db";

async function hasSeedTicker(ticker: string): Promise<boolean> {
  return withDb(async (c) => {
    const { rows } = await c.query<{ n: string }>(
      `SELECT COUNT(*)::text AS n FROM tms.tickers WHERE ticker = $1`,
      [ticker],
    );
    return Number(rows[0].n) > 0;
  });
}

test.describe("session gap heatmap", () => {
  test("renders the heatmap for a ticker with bars", async ({ page }) => {
    test.skip(
      (await withDb((c) => tableTruth(c, "bars_daily"))).rows === 0,
      "no bars in DB — nothing to visualize",
    );

    await page.goto("/systems?tab=data");

    // Prefer the seeded GAPPY (deterministic gaps); else fall back to whatever
    // ticker the coverage table's inspect button hands the heatmap.
    const useGappy = await hasSeedTicker("GAPPY");

    if (useGappy) {
      await page.getByTestId("gap-ticker-input").fill("GAPPY");
      await page.getByTestId("gap-ticker-submit").click();
    } else {
      await page.getByTestId("coverage-inspect-gaps").click();
    }

    const heatmap = page.getByTestId("gap-heatmap");
    await expect(heatmap).toBeVisible();

    // The span badge + at least one month grid must render.
    await expect(page.getByTestId("gap-span")).toBeVisible();
    await expect(
      page.locator('[data-testid^="gap-month-"]').first(),
    ).toBeVisible();

    // At least one calendar cell exists (missing or present).
    const cells = page.locator(
      '[data-testid="gap-cell"], [data-testid="gap-cell-missing"]',
    );
    await expect(cells.first()).toBeVisible();
    expect(await cells.count()).toBeGreaterThan(0);

    if (useGappy) {
      // GAPPY is seeded with missing sessions -> red cells + a missing count.
      await expect(page.getByTestId("gap-missing-count")).toBeVisible();
      await expect(
        page.locator('[data-testid="gap-cell-missing"]').first(),
      ).toBeVisible();
    }
  });

  test("clean ticker renders a complete (no-missing) heatmap", async ({
    page,
  }) => {
    test.skip(
      !(await hasSeedTicker("CLEAN")),
      "CLEAN seed ticker absent (running against real data)",
    );

    await page.goto("/systems?tab=data");
    await page.getByTestId("gap-ticker-input").fill("CLEAN");
    await page.getByTestId("gap-ticker-submit").click();

    await expect(page.getByTestId("gap-heatmap")).toBeVisible();
    await expect(page.getByTestId("gap-no-missing")).toBeVisible();
    await expect(page.locator('[data-testid="gap-cell-missing"]')).toHaveCount(
      0,
    );
  });

  test("unknown ticker surfaces the not-found empty state", async ({ page }) => {
    await page.goto("/systems?tab=data");
    await page.getByTestId("gap-ticker-input").fill("ZZZ_NO_SUCH_TICKER");
    await page.getByTestId("gap-ticker-submit").click();
    await expect(page.getByTestId("gap-not-found")).toBeVisible();
  });
});
