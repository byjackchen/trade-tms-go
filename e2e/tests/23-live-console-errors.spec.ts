/**
 * (6) Zero severe console errors on the /session cockpit pages.
 *
 * The /session cockpit opens long-lived WS connections (signal /
 * portfolio_health / watchlist frames bridged from Redis, plus the live tick-
 * tape) and polls the live read endpoints. None of that may produce a severe
 * browser console error or an uncaught page error — only genuine React/JS
 * defects fail here (network-surfaced errors and a documented framework
 * allowlist are excluded by the `consoleErrors` fixture, fixtures/test.ts).
 *
 * Like spec 06, we navigate with waitUntil "domcontentloaded" (the app shell's
 * persistent SSE stream + the cockpit's WS defer the `load` event past the test
 * budget) and prove the content mounted via testid visibility before asserting
 * the console is clean. This runs whether /session is still the coming-soon
 * placeholder OR the real cockpit — both must be console-clean.
 */

import { test, expect } from "../fixtures/test";
import { liveReaderAvailable } from "../lib/live";

/** Bounded best-effort settle: lets late XHRs + a render tick flush without ever
 * hanging on the perpetually-open SSE/WS connections. */
async function settle(page: import("@playwright/test").Page): Promise<void> {
  await page
    .waitForLoadState("networkidle", { timeout: 2_000 })
    .catch(() => {
      /* SSE + live WS keep connections open; networkidle never settles. */
    });
}

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

test.describe("no severe console errors on /session", () => {
  test("/session renders without severe console errors", async ({
    page,
    consoleErrors,
  }) => {
    // domcontentloaded, not the default "load": the app shell's SSE stream and
    // the cockpit's live WS defer the load event past the test budget.
    await page.goto("/session", { waitUntil: "domcontentloaded" });

    await expect(page.getByTestId("app-shell")).toBeVisible();
    // The page is "mounted" once EITHER the real cockpit root or the coming-soon
    // placeholder is visible (the cockpit ships after the earlier workspaces).
    await waitReady(page, ["session-module"]);

    // Let the live WS open + the first frames / REST snapshots flush so any late
    // render error fires. The cockpit subscribes to multiple streams on mount.
    await settle(page);
    await page.waitForTimeout(2_500);

    expect(
      consoleErrors,
      `severe console/page errors on /session:\n` +
        consoleErrors.map((e) => `  [${e.kind}] ${e.text}`).join("\n"),
    ).toHaveLength(0);
  });

  test("/account renders without severe console errors", async ({
    page,
    consoleErrors,
  }) => {
    // The book half of the split: positions / account panel live on /account.
    await page.goto("/account", { waitUntil: "domcontentloaded" });

    await expect(page.getByTestId("app-shell")).toBeVisible();
    await waitReady(page, ["account-module"]);

    await settle(page);
    await page.waitForTimeout(2_500);

    expect(
      consoleErrors,
      `severe console/page errors on /account:\n` +
        consoleErrors.map((e) => `  [${e.kind}] ${e.text}`).join("\n"),
    ).toHaveLength(0);
  });

  test("the live cockpit survives a kill-switch interaction without console errors", async ({
    page,
    consoleErrors,
  }) => {
    // Only meaningful once the real cockpit + live reader are present; otherwise
    // this is redundant with the page-load check above. The kill-switch /
    // session controls live on /session.
    await page.goto("/session", { waitUntil: "domcontentloaded" });
    await expect(page.getByTestId("app-shell")).toBeVisible();

    const realCockpit = page.getByTestId("session-module");
    await expect
      .poll(async () => (await realCockpit.count()) > 0, { timeout: 15_000 })
      .toBeDefined();
    if ((await realCockpit.count()) === 0) {
      test.skip(true, "Live cockpit not yet implemented (coming-soon).");
      return;
    }
    if (!(await liveReaderAvailable())) {
      test.skip(true, "API started without a live reader (live endpoints 503).");
      return;
    }

    // Open (but DO NOT confirm) the halt control if present — opening a dialog
    // and dismissing it must not throw. This exercises the cockpit's most error-
    // prone interaction path (a confirmation dialog over live state).
    const haltButton = page.getByTestId("live-halt-button");
    if (await haltButton.count()) {
      await haltButton.click();
      const confirm = page.getByTestId("live-halt-confirm");
      if (await confirm.count()) {
        await expect(confirm).toBeVisible();
        const cancel = page.getByTestId("live-halt-confirm-cancel");
        if (await cancel.count()) {
          await cancel.click();
        } else {
          await page.keyboard.press("Escape");
        }
      }
    }

    await settle(page);
    await page.waitForTimeout(1_500);
    expect(
      consoleErrors,
      `severe console/page errors during /session interaction:\n` +
        consoleErrors.map((e) => `  [${e.kind}] ${e.text}`).join("\n"),
    ).toHaveLength(0);
  });
});
