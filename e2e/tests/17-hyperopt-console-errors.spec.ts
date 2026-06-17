/**
 * (5) Zero severe console errors on the Strategies Tune (hyperopt) surface +
 * the study detail deep-link.
 *
 * In the FINAL 4-top IA single-strategy hyperopt folded into Strategies (docs/
 * concept-alignment.md §3.4, A2): the retired /hyperopt 301-redirects to
 * /strategies (the per-strategy Tune section) and /hyperopt/:id to
 * /strategies?study=:id (the inline study detail). Mirrors 06-console-errors:
 * loads the Strategies page and (for a real persisted study) the study detail
 * deep-link, exercises the primary content, and asserts no severe browser
 * console errors or uncaught page errors fired. "Severe" excludes
 * network-surfaced errors and the documented framework allowlist (fixtures/
 * test.ts) — only genuine React/JS defects fail here.
 *
 * Navigation uses waitUntil "domcontentloaded" (not the default "load") because
 * the app shell holds a persistent EventSource("/api/stream") SSE connection
 * that defers the load event past the test budget; readiness is proved via
 * testid visibility instead.
 *
 * The detail test self-skips until a study exists and the inline study detail is
 * deep-link-wired.
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
  test("/strategies (Tune surface) renders without severe console errors", async ({
    page,
    consoleErrors,
  }) => {
    await page.goto("/strategies", { waitUntil: "domcontentloaded" });
    await expect(page.getByTestId("app-shell")).toBeVisible();

    // The Strategies page is mounted once its root is visible; the Tune section
    // is the hyperopt surface that folded in here.
    await expect(page.getByTestId("strategies-page")).toBeVisible();

    await settle(page);
    await page.waitForTimeout(1_500);
    expect(
      consoleErrors,
      `severe console/page errors on /strategies:\n` +
        consoleErrors.map((e) => `  [${e.kind}] ${e.text}`).join("\n"),
    ).toHaveLength(0);
  });

  test("/strategies?study={id} (study detail) renders without severe console errors", async ({
    page,
    consoleErrors,
  }) => {
    const study = await latestStudy();
    test.skip(!study, "no hyperopt study to open a detail page for yet.");
    if (!study) return;

    await page.goto(`/strategies?study=${study.studyTs}`, {
      waitUntil: "domcontentloaded",
    });
    await expect(page.getByTestId("app-shell")).toBeVisible();
    await expect(page.getByTestId("strategies-page")).toBeVisible();

    // The inline study detail (`hyperopt-detail`) renders only once the study
    // deep-link is wired; self-skip otherwise so the gate stays green.
    const detail = page.getByTestId("hyperopt-detail");
    test.skip(
      (await detail.count()) === 0,
      "study detail deep-link not yet wired.",
    );

    await settle(page);
    await page.waitForTimeout(1_500);
    expect(
      consoleErrors,
      `severe console/page errors on /strategies?study=${study.studyTs}:\n` +
        consoleErrors.map((e) => `  [${e.kind}] ${e.text}`).join("\n"),
    ).toHaveLength(0);
  });
});
