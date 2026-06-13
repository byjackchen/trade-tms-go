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
  /** Stable testids; the page is "mounted" once ANY of them is visible (the
   * Backtests section may render either the coming-soon placeholder or the real
   * workspace root, depending on build order). */
  ready: string[];
};

const ROUTES: Route[] = [
  { path: "/data", ready: ["data-page"] },
  { path: "/backtests", ready: ["backtests-placeholder", "backtests-page"] },
  { path: "/hyperopt", ready: ["hyperopt-placeholder"] },
  { path: "/live", ready: ["live-placeholder"] },
  { path: "/ops", ready: ["ops-placeholder"] },
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

  // (5) The backtest DETAIL route must also be free of severe console errors,
  // for a real persisted run. Self-skips until there is a COMPLETE run and the
  // detail page is implemented.
  test("/backtests/{id} renders without severe console errors", async ({
    page,
    consoleErrors,
  }) => {
    const run = await withDb((c) => latestCompleteRun(c));
    test.skip(!run, "no COMPLETE run to open a detail page for yet.");
    if (!run) return;

    await page.goto(`/backtests/${run.id}`, { waitUntil: "domcontentloaded" });
    await expect(page.getByTestId("app-shell")).toBeVisible();

    const detail = page.getByTestId("backtest-detail");
    const placeholder = page.getByTestId("backtests-placeholder");
    await expect
      .poll(
        async () => (await detail.count()) + (await placeholder.count()),
        { timeout: 15_000 },
      )
      .toBeGreaterThan(0);
    test.skip(
      (await detail.count()) === 0,
      "Backtests detail page not yet implemented.",
    );

    await settle(page);
    await page.waitForTimeout(1_500);
    expect(
      consoleErrors,
      `severe console/page errors on /backtests/${run.id}:\n` +
        consoleErrors.map((e) => `  [${e.kind}] ${e.text}`).join("\n"),
    ).toHaveLength(0);
  });

  test("root redirects to /data without severe console errors", async ({
    page,
    consoleErrors,
  }) => {
    await page.goto("/", { waitUntil: "domcontentloaded" });
    await expect(page).toHaveURL(/\/data$/);
    await expect(page.getByTestId("data-page")).toBeVisible();
    await settle(page);
    await page.waitForTimeout(1_000);
    expect(
      consoleErrors,
      consoleErrors.map((e) => `[${e.kind}] ${e.text}`).join("\n"),
    ).toHaveLength(0);
  });

  test("the Data page sync-runs + watermarks tables render", async ({
    page,
  }) => {
    // A light smoke that the sync history surface mounts (used by the refresh
    // flow's "sync-runs table gains a row" assertion path).
    await page.goto("/data", { waitUntil: "domcontentloaded" });
    await expect(page.getByTestId("sync-runs-card")).toBeVisible();
  });
});
