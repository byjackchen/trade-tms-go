/**
 * (a) The Data page renders coverage that MATCHES ground truth.
 *
 * The test queries postgres directly (independently of the Go API) and asserts
 * the numbers the UI paints — rows and distinct tickers per coverage table —
 * equal the DB's own counts. A divergence here means the API mis-counted or the
 * UI mis-rendered; either is a real defect.
 */

import { test, expect } from "../fixtures/test";
import { withDb, tableTruth } from "../lib/db";
import { formatIntLikeUi } from "../lib/format";

// The four tables the coverage endpoint reports, in contract order.
const TABLES = ["tickers", "bars_daily", "fundamentals_sf1", "events"] as const;

test.describe("data coverage matches DB ground truth", () => {
  test("each coverage row's rows/tickers equal the DB counts", async ({
    page,
  }) => {
    // 1. Compute ground truth straight from postgres.
    const truth = await withDb(async (c) => {
      const out: Record<string, { rows: number; tickers: number }> = {};
      for (const t of TABLES) out[t] = await tableTruth(c, t);
      return out;
    });

    // 2. Load the Data page and wait for the coverage table to populate.
    await page.goto("/data");
    const coverage = page.getByTestId("coverage-table");
    await expect(coverage).toBeVisible();

    // 3. For every table, assert the rendered rows/tickers == DB counts.
    for (const t of TABLES) {
      const row = page.getByTestId(`coverage-row-${t}`);
      await expect(row, `coverage row for ${t} should render`).toBeVisible();

      const nameCell = row.getByTestId("coverage-table-name");
      await expect(nameCell).toHaveText(t);

      const rowsCell = row.getByTestId("coverage-rows");
      const tickersCell = row.getByTestId("coverage-tickers");

      await expect(
        rowsCell,
        `${t}.rows rendered should equal DB COUNT(*) = ${truth[t].rows}`,
      ).toHaveText(formatIntLikeUi(truth[t].rows));

      await expect(
        tickersCell,
        `${t}.tickers rendered should equal DB COUNT(DISTINCT ticker) = ${truth[t].tickers}`,
      ).toHaveText(formatIntLikeUi(truth[t].tickers));
    }
  });

  test("the bars_daily row exposes gap detection + an inspect action", async ({
    page,
  }) => {
    await page.goto("/data");
    const bars = page.getByTestId("coverage-row-bars_daily");
    await expect(bars).toBeVisible();

    // The bars_daily row is the only one offering a "Gaps" inspect button.
    await expect(bars.getByTestId("coverage-inspect-gaps")).toBeVisible();

    // The gaps cell renders one of the two known badges (clean or present).
    const gapsCell = bars.getByTestId("coverage-gaps");
    await expect(gapsCell).toBeVisible();
    const clean = gapsCell.getByTestId("gaps-clean");
    const present = gapsCell.getByTestId("gaps-present");
    await expect(
      (await clean.count()) + (await present.count()),
      "bars_daily gaps cell renders either a clean or present badge",
    ).toBeGreaterThan(0);
  });
});
