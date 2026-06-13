/**
 * (5) Zero severe console errors on /hyperopt + /hyperopt/{id}.
 *
 * Mirrors 06-console-errors: loads the Hyperopt list and (for a real persisted
 * study) the detail page, exercises the primary content, and asserts no severe
 * browser console errors or uncaught page errors fired. "Severe" excludes
 * network-surfaced errors and the documented framework allowlist (fixtures/
 * test.ts) — only genuine React/JS defects fail here.
 *
 * Navigation uses waitUntil "domcontentloaded" (not the default "load") because
 * the app shell holds a persistent EventSource("/api/stream") SSE connection
 * that defers the load event past the test budget; readiness is proved via
 * testid visibility instead.
 *
 * The list test runs whether the workspace is coming-soon or real (the
 * placeholder must also be error-free); the detail test self-skips until a study
 * exists and the detail page is implemented.
 */

import { test, expect } from "../fixtures/test";
import { latestStudy } from "../lib/hyperopt";

/** Bounded best-effort settle: lets late XHRs + a render tick flush without
 * ever hanging on the perpetually-open SSE connection. */
async function settle(page: import("@playwright/test").Page): Promise<void> {
  await page.waitForLoadState("networkidle", { timeout: 2_000 }).catch(() => {
    /* SSE keeps a connection open; networkidle never settles — expected. */
  });
}

test.describe("hyperopt: no severe console errors", () => {
  test("/hyperopt renders without severe console errors", async ({
    page,
    consoleErrors,
  }) => {
    await page.goto("/hyperopt", { waitUntil: "domcontentloaded" });
    await expect(page.getByTestId("app-shell")).toBeVisible();

    // The page is mounted once either the real workspace root or the coming-soon
    // placeholder is visible.
    const real = page.getByTestId("hyperopt-page");
    const placeholder = page.getByTestId("hyperopt-placeholder");
    await expect
      .poll(
        async () =>
          (await real.isVisible().catch(() => false)) ||
          (await placeholder.isVisible().catch(() => false)),
        { timeout: 15_000 },
      )
      .toBe(true);

    await settle(page);
    await page.waitForTimeout(1_500);
    expect(
      consoleErrors,
      `severe console/page errors on /hyperopt:\n` +
        consoleErrors.map((e) => `  [${e.kind}] ${e.text}`).join("\n"),
    ).toHaveLength(0);
  });

  test("/hyperopt/{id} renders without severe console errors", async ({
    page,
    consoleErrors,
  }) => {
    const study = await latestStudy();
    test.skip(!study, "no hyperopt study to open a detail page for yet.");
    if (!study) return;

    await page.goto(`/hyperopt/${study.studyTs}`, {
      waitUntil: "domcontentloaded",
    });
    await expect(page.getByTestId("app-shell")).toBeVisible();

    const detail = page.getByTestId("hyperopt-detail");
    const placeholder = page.getByTestId("hyperopt-placeholder");
    await expect
      .poll(
        async () => (await detail.count()) + (await placeholder.count()),
        { timeout: 15_000 },
      )
      .toBeGreaterThan(0);
    test.skip(
      (await detail.count()) === 0,
      "Hyperopt detail page not yet implemented.",
    );

    await settle(page);
    await page.waitForTimeout(1_500);
    expect(
      consoleErrors,
      `severe console/page errors on /hyperopt/${study.studyTs}:\n` +
        consoleErrors.map((e) => `  [${e.kind}] ${e.text}`).join("\n"),
    ).toHaveLength(0);
  });
});
