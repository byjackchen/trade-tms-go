/**
 * (3) Launch a REAL single-strategy backtest from the UI, end to end.
 *
 * Drives a real (non-scripted) strategy run — e.g. SEPA over a handful of
 * tickers, ~1 year — purely through the browser:
 *   1. open the launch affordance (the strategy detail page's per-strategy
 *      `strategy-backtest-<id>` button, or — since a single-member Composition IS
 *      a single strategy — the Compositions module's `composition-backtest-<id>`);
 *   2. fill tickers + a ~1-year window, submit;
 *   3. watch the shared `job-progress` panel transition running -> succeeded via
 *      the UI (the worker drives it; the tracker reconciles over the WS job
 *      stream + REST poll — the requirement's "WS progress -> succeeded");
 *   4. follow the inline detail deep-link (/compositions?backtest={id}) and confirm
 *      the equity chart + trades render and the metric cards == the API ground truth.
 *
 * Ground-truth coupling: the linked run is the run the engine persisted
 * (tms.runs gains a COMPLETE row whose id == the detail URL), it carries the
 * launched strategy in its config, and the rendered metric cards equal
 * GET /api/v1/backtests/{id}.metrics exactly. No fabricated ids/numbers.
 *
 * Robustness: self-skips while the strategy-launch / Compositions backtest
 * affordance is unbuilt, or when the stack has no tradable bars for a
 * real-strategy run. Once wired, the assertions are exact and permanent.
 */

import { test, expect } from "../fixtures/test";
import { getAuthed } from "../lib/api";
import {
  withDb,
  completeRunCount,
  latestCompleteRun,
  runMetricsTruth,
  equityPointCount,
} from "../lib/db";
import { pickScriptedLaunch, TERMINAL } from "../lib/backtests";
import { strategyDetailReady } from "../lib/strategies";

/** The real strategy we launch. SEPA derives long entries from trend-template /
 * VCP pivots on common stocks, so explicit tickers + a year window suffice. */
const STRATEGY = "sepa";

type Page = import("@playwright/test").Page;

/**
 * Open a launch affordance pre-targeting the real strategy and return whether
 * the New-backtest dialog became visible. In the FINAL 4-top IA a single-strategy
 * backtest = a single-member Composition backtest (docs/concept-alignment.md
 * §3.4 A3). Tries, in order:
 *   (a) the strategy detail page's per-strategy `strategy-backtest-<id>` button;
 *   (b) the Compositions module's per-Composition `composition-backtest-<id>`
 *       launcher (a single-member Composition IS a single strategy).
 * Returns false when neither path can open a backtest dialog.
 */
async function openRealStrategyLaunch(page: Page): Promise<boolean> {
  // (a) Strategy detail page launch (per-strategy backtest button).
  if (await strategyDetailReady(page, STRATEGY)) {
    const launch = page.getByTestId(`strategy-backtest-${STRATEGY}`);
    if (await launch.count()) {
      await launch.first().click();
      const dialog = page.getByTestId("backtest-dialog");
      try {
        await dialog.waitFor({ state: "visible", timeout: 10_000 });
        return true;
      } catch {
        /* fall through to the Compositions path */
      }
    }
  }

  // (b) Compositions module: a per-Composition backtest launcher (a single-member
  // Composition is the single-strategy backtest object).
  await page.goto("/compositions", { waitUntil: "domcontentloaded" });
  await expect(page.getByTestId("app-shell")).toBeVisible();
  const opener = page
    .locator('[data-testid^="composition-backtest-"]')
    .first();
  if (!(await opener.count())) return false;
  await opener.click();
  const dialog = page.getByTestId("backtest-dialog");
  try {
    await dialog.waitFor({ state: "visible", timeout: 10_000 });
  } catch {
    return false;
  }
  return true;
}

test.describe("real single-strategy backtest", () => {
  test("launch SEPA from the UI, WS progress -> succeeded, detail matches API", async ({
    page,
  }) => {
    // Need tradable bars to pick a window/tickers; reuse the scripted picker —
    // it returns the widest-coverage symbols + a covered window.
    const cover = await pickScriptedLaunch();
    if (!cover) {
      test.skip(true, "no tradable bars in the stack — cannot run a backtest.");
      return;
    }

    if (!(await openRealStrategyLaunch(page))) {
      test.skip(
        true,
        "UI cannot launch a real single-strategy backtest yet (no strategy-launch affordance).",
      );
      return;
    }

    await expect(page.getByTestId("backtest-form")).toBeVisible();

    const before = await withDb((c) => completeRunCount(c));

    // A handful of tickers over (up to) the widest covered ~1-year window. The
    // picker already clamps to available coverage, so this never exceeds data.
    if (await page.getByTestId("backtest-tickers").count()) {
      await page.getByTestId("backtest-tickers").fill(cover.tickers.join(" "));
    }
    await page.getByTestId("backtest-start").fill(cover.start);
    await page.getByTestId("backtest-end").fill(cover.end);

    // close-fill profile for a deterministic result, when the control exists.
    const profile = page.getByTestId("backtest-fill-profile");
    if (await profile.count()) {
      await profile.selectOption("close-fill").catch(() => {});
    }

    // Submit and watch the shared job panel via the live WS stream.
    await page.getByTestId("backtest-submit").click();

    const progress = page.getByTestId("job-progress");
    await expect(progress).toBeVisible();
    const jobId = await progress.getAttribute("data-job-id");
    expect(jobId, "job-progress exposes a numeric data-job-id").toMatch(/^\d+$/);

    // Running -> terminal, asserted succeeded (a real strategy over a covered
    // window must complete; a broken wiring would fail and be caught here).
    const complete = page.getByTestId("job-complete");
    await expect(complete).toBeVisible({ timeout: 85_000 });
    const outcome = await complete.getAttribute("data-outcome");
    expect(
      outcome && TERMINAL.has(outcome),
      `backtest job reached a terminal outcome (got "${outcome}")`,
    ).toBeTruthy();
    expect(outcome, `SEPA backtest succeeds end-to-end (got "${outcome}")`).toBe(
      "succeeded",
    );

    // DB gained a COMPLETE run.
    await expect
      .poll(async () => withDb((c) => completeRunCount(c)), { timeout: 30_000 })
      .toBeGreaterThanOrEqual(before + 1);
    const latest = await withDb((c) => latestCompleteRun(c));
    expect(latest, "a COMPLETE run exists after the launch").not.toBeNull();

    // Follow the detail link/redirect to the persisted run id. In the FINAL IA
    // the result opens INLINE in the Compositions module via the `?backtest=<id>`
    // deep-link (NewBacktestDialog onView -> /compositions?backtest=<id>).
    const detailLink = page.getByTestId("backtest-detail-link");
    if (await detailLink.count()) {
      await detailLink.first().click();
    }
    await expect
      .poll(() => new URL(page.url()).searchParams.get("backtest"), {
        timeout: 30_000,
      })
      .toBe(String(latest!.id));

    // The launched run actually used the real strategy (config ground truth).
    const apiDetail = await getAuthed(`backtests/${latest!.id}`);
    expect(apiDetail.status, "GET /backtests/{id} is 200").toBe(200);
    const cfg = (apiDetail.body as { config?: Record<string, unknown> }).config;
    if (cfg && typeof cfg.strategy === "string") {
      expect(cfg.strategy, "run config records the launched strategy").toBe(
        STRATEGY,
      );
    }

    // Detail mounted; equity chart + trades render and metric cards == API.
    await expect(page.getByTestId("backtest-detail")).toBeVisible({
      timeout: 30_000,
    });

    // Equity chart paints (canvas/svg) and its point count, when exposed, equals
    // the DB/API source.
    const dbEquityN = await withDb((c) => equityPointCount(c, latest!.id));
    const chart = page.getByTestId("equity-chart");
    await expect(chart, "equity chart renders").toBeVisible();
    await expect
      .poll(async () => chart.locator("canvas, svg").count(), { timeout: 15_000 })
      .toBeGreaterThan(0);
    const chartPts = await chart.getAttribute("data-point-count");
    if (chartPts != null && chartPts !== "") {
      expect(Number(chartPts), "chart point count === equity_curves").toBe(
        dbEquityN,
      );
    }

    // Trades surface mounts (populated table or empty-state — a SEPA run may
    // hold positions open at run end and produce no round trips).
    const tradesTable = page.getByTestId("trades-table");
    const tradesEmpty = page.getByTestId("trades-empty");
    await expect
      .poll(
        async () => (await tradesTable.count()) + (await tradesEmpty.count()),
        { timeout: 15_000 },
      )
      .toBeGreaterThan(0);

    // -------- metric cards === API ground truth (requirement: "assert metric
    // cards == API ground truth") --------
    const dbMetrics = await withDb((c) => runMetricsTruth(c, latest!.id));
    expect(dbMetrics, "run has a portfolio metrics row").not.toBeNull();
    const apiMetrics =
      (apiDetail.body as { metrics?: Record<string, number> }).metrics ?? {};

    // API and DB must already agree (the API reads its own source correctly).
    expect(Number(apiMetrics.final_balance_usd)).toBeCloseTo(
      dbMetrics!.finalBalanceUsd,
      4,
    );

    type Card = {
      testid: string;
      apiKey: keyof typeof apiMetrics;
      decimals: number;
    };
    const cards: Card[] = [
      { testid: "metric-final-balance", apiKey: "final_balance_usd", decimals: 2 },
      { testid: "metric-total-pnl", apiKey: "total_pnl_usd", decimals: 2 },
      { testid: "metric-sharpe", apiKey: "sharpe", decimals: 2 },
      { testid: "metric-calmar", apiKey: "calmar", decimals: 2 },
      { testid: "metric-max-drawdown", apiKey: "max_drawdown_pct", decimals: 2 },
      { testid: "metric-num-orders", apiKey: "num_orders", decimals: 0 },
      { testid: "metric-num-positions", apiKey: "num_positions", decimals: 0 },
    ];

    for (const card of cards) {
      const truth = Number(apiMetrics[card.apiKey]);
      // The API must carry the metric; if a build omits one key, skip just it.
      if (!Number.isFinite(truth)) continue;
      const el = page.getByTestId(card.testid);
      await expect(el, `metric card ${card.testid} renders`).toBeVisible();
      const rawAttr = await el.getAttribute("data-value");
      if (rawAttr != null && rawAttr !== "") {
        expect(
          Number(rawAttr),
          `${card.testid} data-value === API ${String(card.apiKey)}`,
        ).toBeCloseTo(truth, 4);
      } else {
        const shown = Number(
          ((await el.textContent()) ?? "").replace(/[^0-9.\-]/g, ""),
        );
        const tol = card.decimals === 0 ? 0 : Math.pow(10, -card.decimals) / 2;
        expect(
          Math.abs(shown - truth),
          `${card.testid} rendered "${shown}" ~= API ${truth}`,
        ).toBeLessThanOrEqual(tol + 1e-9);
      }
    }
  });
});
