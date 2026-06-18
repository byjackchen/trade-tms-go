/**
 * (f) Zero severe console errors on all pages.
 *
 * For every route in the app we load the page, exercise its primary content,
 * and assert no severe browser console errors or uncaught page errors fired.
 * "Severe" excludes network-surfaced errors and a small documented framework
 * allowlist (see fixtures/test.ts) — only genuine React/JS defects fail here.
 *
 * The `consoleErrors` fixture attaches its listeners before navigation, so it
 * captures errors from the very first paint onward.
 *
 * Navigation note: every page mounts the app shell, which opens a persistent
 * `EventSource("/api/stream")` SSE connection (see use-job-stream.ts). That
 * long-lived response defers the page `load` event indefinitely, so a default
 * `page.goto` (waitUntil "load") would block until the navigation timeout and
 * blow the per-test budget. We navigate with waitUntil "domcontentloaded" —
 * the correct readiness signal for an SSE-bearing page — and then prove the
 * content mounted via testid visibility. A short bounded "networkidle" settle
 * (which the open SSE stream prevents from ever firing) is wrapped so it can
 * never consume the whole timeout.
 */

import { test, expect } from "../fixtures/test";
import { withDb, latestCompleteRun } from "../lib/db";
import { strategiesUiReady, strategyDetailReady, STRATEGY_IDS } from "../lib/strategies";

/** Bounded best-effort settle: lets late XHRs + a render tick flush without
 * ever hanging on the perpetually-open SSE connection. */
async function settle(page: import("@playwright/test").Page): Promise<void> {
  await page
    .waitForLoadState("networkidle", { timeout: 2_000 })
    .catch(() => {
      /* SSE keeps a connection open; networkidle never settles — expected. */
    });
}

type Route = {
  path: string;
  /** Stable testids; the page is "mounted" once ANY of them is visible. */
  ready: string[];
};

// The FINAL 5-top IA (docs/concept-alignment.md §3.4, C7): Systems & Data →
// Strategies → Compositions → Session → Account. The old single "Trade" top-level
// was SPLIT into Session (/session, runtime control) and Account (/account, the
// book); the retired 6-top routes (/data,/backtests,/hyperopt,/ops) and the
// interim (/paper,/live,/trade) 301-redirect onto these (see ui/next.config.ts).
// Each `ready` testid is the page's main-content root as rendered today
// (ui/src/app/*/page.tsx); Session's is the <SessionModule> wrapper
// (`session-module`) + its header (`session-header`), and Account's is the
// <AccountModule> wrapper (`account-module`) + its header (`account-header`).
const ROUTES: Route[] = [
  { path: "/systems", ready: ["systems-page"] },
  { path: "/strategies", ready: ["strategies-page"] },
  { path: "/compositions", ready: ["compositions-page"] },
  { path: "/session", ready: ["session-module", "session-header"] },
  { path: "/account", ready: ["account-module", "account-header"] },
];

/** Wait for the first of the candidate readiness testids to become visible. */
async function waitReady(
  page: import("@playwright/test").Page,
  ready: string[],
): Promise<void> {
  await expect
    .poll(
      async () => {
        for (const id of ready) {
          if (await page.getByTestId(id).first().isVisible()) return true;
        }
        return false;
      },
      { timeout: 15_000 },
    )
    .toBe(true);
}

test.describe("no severe console errors", () => {
  for (const route of ROUTES) {
    test(`${route.path} renders without severe console errors`, async ({
      page,
      consoleErrors,
    }) => {
      // domcontentloaded, not the default "load": the app shell's persistent
      // SSE stream defers the load event past the test budget.
      await page.goto(route.path, { waitUntil: "domcontentloaded" });

      // Confirm the app shell and the page's main content mounted.
      await expect(page.getByTestId("app-shell")).toBeVisible();
      await waitReady(page, route.ready);

      // Let async data fetches + a render tick flush so any late error fires.
      await settle(page);
      await page.waitForTimeout(1_500);

      expect(
        consoleErrors,
        `severe console/page errors on ${route.path}:\n` +
          consoleErrors.map((e) => `  [${e.kind}] ${e.text}`).join("\n"),
      ).toHaveLength(0);
    });
  }

  // (5) The backtest DETAIL view must also be free of severe console errors, for
  // a real persisted run. In the 4-top IA a backtest's object is always a
  // Composition: the retired `/backtests/:id` route 301-redirects to
  // `/compositions?backtest=:id`, where the inline BacktestPanel (`backtest-detail`)
  // renders. Self-skips until there is a COMPLETE run.
  test("/backtests/{id} (-> /compositions?backtest=) renders without severe console errors", async ({
    page,
    consoleErrors,
  }) => {
    const run = await withDb((c) => latestCompleteRun(c));
    test.skip(!run, "no COMPLETE run to open a detail page for yet.");
    if (!run) return;

    await page.goto(`/backtests/${run.id}`, { waitUntil: "domcontentloaded" });
    await expect(page.getByTestId("app-shell")).toBeVisible();
    // The redirect lands on the Compositions module with the deep-linked panel.
    await expect(page).toHaveURL(new RegExp(`/compositions\\?backtest=${run.id}`));

    const detail = page.getByTestId("backtest-detail");
    await expect
      .poll(async () => detail.count(), { timeout: 15_000 })
      .toBeGreaterThan(0);

    await settle(page);
    await page.waitForTimeout(1_500);
    expect(
      consoleErrors,
      `severe console/page errors on /compositions?backtest=${run.id}:\n` +
        consoleErrors.map((e) => `  [${e.kind}] ${e.text}`).join("\n"),
    ).toHaveLength(0);
  });

  // (4) The Strategies LIST route must be free of severe console errors. Self-
  // skips until the Strategies workspace replaces the coming-soon placeholder /
  // the route is built.
  test("/strategies renders without severe console errors", async ({
    page,
    consoleErrors,
  }) => {
    const ready = await strategiesUiReady(page); // navigates to /strategies
    test.skip(!ready, "Strategies workspace not yet implemented (coming-soon).");
    if (!ready) return;

    await expect(page.getByTestId("app-shell")).toBeVisible();
    await expect(page.getByTestId("strategies-page")).toBeVisible();

    await settle(page);
    await page.waitForTimeout(1_500);
    expect(
      consoleErrors,
      `severe console/page errors on /strategies:\n` +
        consoleErrors.map((e) => `  [${e.kind}] ${e.text}`).join("\n"),
    ).toHaveLength(0);
  });

  // (4) The Strategies DETAIL route must be free of severe console errors, for
  // each canonical strategy. Self-skips until the detail page is implemented.
  test("/strategies/{id} renders without severe console errors", async ({
    page,
    consoleErrors,
  }) => {
    const id = STRATEGY_IDS[0]; // sepa — a representative real strategy
    const ready = await strategyDetailReady(page, id); // navigates to detail
    test.skip(!ready, "Strategies detail page not yet implemented (coming-soon).");
    if (!ready) return;

    await expect(page.getByTestId("app-shell")).toBeVisible();
    await expect(page.getByTestId("strategy-detail")).toBeVisible();

    await settle(page);
    await page.waitForTimeout(1_500);
    expect(
      consoleErrors,
      `severe console/page errors on /strategies/${id}:\n` +
        consoleErrors.map((e) => `  [${e.kind}] ${e.text}`).join("\n"),
    ).toHaveLength(0);
  });

  test("root redirects to /systems without severe console errors", async ({
    page,
    consoleErrors,
  }) => {
    // The 4-top IA lands the root on Systems & Data (ui/src/app/page.tsx).
    await page.goto("/", { waitUntil: "domcontentloaded" });
    await expect(page).toHaveURL(/\/systems$/);
    await expect(page.getByTestId("systems-page")).toBeVisible();
    await settle(page);
    await page.waitForTimeout(1_000);
    expect(
      consoleErrors,
      consoleErrors.map((e) => `[${e.kind}] ${e.text}`).join("\n"),
    ).toHaveLength(0);
  });

  test("the Data tab sync-runs + watermarks tables render", async ({
    page,
  }) => {
    // A light smoke that the sync history surface mounts (used by the refresh
    // flow's "sync-runs table gains a row" assertion path). Data now lives as the
    // `?tab=data` tab of Systems & Data; the retired /data route 301s here.
    await page.goto("/systems?tab=data", { waitUntil: "domcontentloaded" });
    await expect(page.getByTestId("sync-runs-card")).toBeVisible();
  });
});
