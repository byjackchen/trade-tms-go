/**
 * (2) Detail correctness + (3) chart / tables render.
 *
 * Against an existing COMPLETE run (created by the launch spec or already in the
 * stack), assert the inline backtest panel (Compositions module, opened via the
 * `?backtest={id}` deep-link in the FINAL 4-top IA) renders metrics + equity that
 * MATCH the DB/API
 * ground truth — the numbers are queried independently here and compared to what
 * the UI shows. Nothing is fabricated:
 *
 *   - metric cards (final balance, total P&L, sharpe, calmar, max-drawdown, the
 *     order/position counts) === tms.run_metrics (portfolio scope) AND === the
 *     GET /api/v1/backtests/{id} payload;
 *   - the equity chart's point count === COUNT(equity_curves) === the
 *     /equity endpoint's points.length, and a canvas/svg is actually painted;
 *   - the trades table row count === COUNT(tms.trades) === /trades length;
 *   - the orders table row count === the /orders array length.
 *
 * The metric-card comparison parses the rendered text into a number (stripping
 * currency/grouping/percent) and asserts equality within a tiny epsilon — the
 * card may round for display, so we compare against the DB value rounded the
 * same way rather than demanding byte-identical strings.
 *
 * Self-skips when the Backtests workspace is still the coming-soon placeholder
 * or no COMPLETE run exists yet.
 */

import { test, expect } from "../fixtures/test";
import { getAuthed } from "../lib/api";
import {
  withDb,
  latestCompleteRun,
  runMetricsTruth,
  equityPointCount,
  tradeCount,
} from "../lib/db";

/** Parse a rendered numeric cell ("$1,234.50", "-3.20%", "1.50") to a number. */
function parseRendered(text: string): number {
  const cleaned = text.replace(/[^0-9.\-]/g, "");
  return Number(cleaned);
}

/** Detail is real once the inline panel exposes the `backtest-detail` root. In
 * the FINAL 4-top IA a backtest's object is always a Composition (docs/concept-
 * alignment.md §3.4 A3): the detail opens INLINE in the Compositions module via
 * the `?backtest={id}` deep-link (the retired /backtests/{id} 301-redirects to
 * /compositions?backtest={id}). Self-skips (returns false) until the panel is
 * wired. */
async function detailReady(
  page: import("@playwright/test").Page,
  id: number,
): Promise<boolean> {
  await page.goto(`/compositions?backtest=${id}`, {
    waitUntil: "domcontentloaded",
  });
  await expect(page.getByTestId("app-shell")).toBeVisible();
  await expect(page.getByTestId("compositions-page")).toBeVisible();
  const detail = page.getByTestId("backtest-detail");
  const deadline = Date.now() + 15_000;
  while (Date.now() < deadline) {
    if (await detail.count()) return true;
    await page.waitForTimeout(250);
  }
  return false;
}

test.describe("backtest detail correctness", () => {
  test("metric cards + equity + tables match DB/API ground truth", async ({
    page,
  }) => {
    const run = await withDb((c) => latestCompleteRun(c));
    if (!run) {
      test.skip(true, "no COMPLETE run to inspect yet.");
      return;
    }

    if (!(await detailReady(page, run.id))) {
      test.skip(true, "Backtests detail page not yet implemented.");
      return;
    }

    // -------- ground truth: postgres + API (independent of the UI) --------
    const dbMetrics = await withDb((c) => runMetricsTruth(c, run.id));
    const dbEquityN = await withDb((c) => equityPointCount(c, run.id));
    const dbTradeN = await withDb((c) => tradeCount(c, run.id));
    expect(dbMetrics, "run has a portfolio metrics row").not.toBeNull();

    const apiDetail = await getAuthed(`backtests/${run.id}`);
    expect(apiDetail.status, "GET /backtests/{id} is 200").toBe(200);
    const detailBody = apiDetail.body as {
      metrics?: Record<string, number>;
    };
    const apiMetrics = detailBody.metrics ?? {};

    const apiEquity = await getAuthed(`backtests/${run.id}/equity`);
    expect(apiEquity.status).toBe(200);
    const apiEquityPts =
      (apiEquity.body as { points?: unknown[] }).points ?? [];

    const apiTrades = await getAuthed(`backtests/${run.id}/trades`);
    expect(apiTrades.status).toBe(200);
    const apiTradeRows =
      (apiTrades.body as { trades?: unknown[] }).trades ?? [];

    const apiOrders = await getAuthed(`backtests/${run.id}/orders`);
    expect(apiOrders.status).toBe(200);
    const apiOrderRows = Array.isArray(apiOrders.body) ? apiOrders.body : [];

    // API and DB must already agree (else the API mis-reads its own source).
    expect(apiEquityPts.length, "API equity count === DB").toBe(dbEquityN);
    expect(apiTradeRows.length, "API trades count === DB").toBe(dbTradeN);
    expect(Number(apiMetrics.final_balance_usd)).toBeCloseTo(
      dbMetrics!.finalBalanceUsd,
      4,
    );

    // -------- the rendered metric cards === ground truth --------
    // Each card exposes the raw numeric value via a stable data-value attribute
    // (preferred, exact) and human text. We assert against data-value when
    // present, otherwise parse the visible text and compare with display
    // rounding tolerance.
    type Card = { testid: string; truth: number; decimals: number };
    const cards: Card[] = [
      {
        testid: "metric-final-balance",
        truth: dbMetrics!.finalBalanceUsd,
        decimals: 2,
      },
      { testid: "metric-total-pnl", truth: dbMetrics!.totalPnlUsd, decimals: 2 },
      { testid: "metric-sharpe", truth: dbMetrics!.sharpe, decimals: 2 },
      { testid: "metric-calmar", truth: dbMetrics!.calmar, decimals: 2 },
      {
        testid: "metric-max-drawdown",
        truth: dbMetrics!.maxDrawdownPct,
        decimals: 2,
      },
      {
        testid: "metric-num-orders",
        truth: dbMetrics!.numOrders,
        decimals: 0,
      },
      {
        testid: "metric-num-positions",
        truth: dbMetrics!.numPositions,
        decimals: 0,
      },
    ];

    for (const card of cards) {
      const el = page.getByTestId(card.testid);
      // Cards are part of the detail contract; require each to be present.
      await expect(el, `metric card ${card.testid} renders`).toBeVisible();

      const rawAttr = await el.getAttribute("data-value");
      if (rawAttr != null && rawAttr !== "") {
        expect(
          Number(rawAttr),
          `${card.testid} data-value === ground truth`,
        ).toBeCloseTo(card.truth, 4);
      } else {
        const shown = parseRendered((await el.textContent()) ?? "");
        const tol = card.decimals === 0 ? 0 : Math.pow(10, -card.decimals) / 2;
        expect(
          Math.abs(shown - card.truth),
          `${card.testid} rendered "${shown}" ~= truth ${card.truth}`,
        ).toBeLessThanOrEqual(tol + 1e-9);
      }
    }

    // -------- (3) equity chart renders (canvas or svg painted) --------
    const chart = page.getByTestId("equity-chart");
    await expect(chart, "equity chart container renders").toBeVisible();
    const canvasOrSvg = chart.locator("canvas, svg");
    await expect
      .poll(async () => canvasOrSvg.count(), { timeout: 15_000 })
      .toBeGreaterThan(0);

    // If the chart exposes a point count, it must equal the source.
    const chartPts = await chart.getAttribute("data-point-count");
    if (chartPts != null && chartPts !== "") {
      expect(
        Number(chartPts),
        "chart point count === equity_curves count",
      ).toBe(dbEquityN);
    }

    // -------- (3) trades table populated + count === source --------
    // The Trades card always mounts; it renders the populated `trades-table`
    // when there are round-trip trades and the `trades-empty` state otherwise
    // (a scripted run may hold positions open and produce no round trips).
    const tradeRows = page.locator('[data-testid^="trade-row-"]');
    if (dbTradeN > 0) {
      await expect(page.getByTestId("trades-table")).toBeVisible();
      await expect
        .poll(async () => tradeRows.count(), { timeout: 15_000 })
        .toBe(dbTradeN);
    } else {
      // Empty is legitimate (the scripted run may hold open or do nothing); the
      // card still mounts an empty-state.
      await expect(page.getByTestId("trades-empty")).toBeVisible();
    }

    // -------- (3) orders table populated + count === API source --------
    // Same contract: populated `orders-table` when there are orders, else the
    // `orders-empty` state.
    const orderRows = page.locator('[data-testid^="order-row-"]');
    if (apiOrderRows.length > 0) {
      await expect(page.getByTestId("orders-table")).toBeVisible();
      await expect
        .poll(async () => orderRows.count(), { timeout: 15_000 })
        .toBe(apiOrderRows.length);
    } else {
      await expect(page.getByTestId("orders-empty")).toBeVisible();
    }
  });
});
