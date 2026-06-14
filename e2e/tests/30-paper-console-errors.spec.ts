/**
 * (7) Zero severe console errors on the paper-trading cockpit.
 *
 * The paper-trading cockpit adds the blotter / positions / account /
 * reconciliation panels and the flatten control on top of the signal cockpit's
 * long-lived WS + polling. None of that may produce a severe browser console
 * error or an uncaught page error — only genuine React/JS defects fail here
 * (network-surfaced errors + a documented framework allowlist are excluded by
 * the `consoleErrors` fixture). This complements spec 23 (signal cockpit) for the
 * P6 trading surface.
 *
 * We exercise the most error-prone interaction the paper cockpit adds — opening
 * (but never confirming) the flatten confirmation dialog over live trading state
 * — and assert the console stays clean. Self-skips cleanly while the panels are
 * not built or the trading reader is absent, but the plain page-load console
 * check runs whenever the cockpit is up.
 */

import { test, expect } from "../fixtures/test";
import { liveTradingAvailable } from "../lib/live";

/** Bounded best-effort settle: lets late XHRs + a render tick flush without ever
 * hanging on the perpetually-open SSE/WS connections. */
async function settle(page: import("@playwright/test").Page): Promise<void> {
  await page.waitForLoadState("networkidle", { timeout: 2_000 }).catch(() => {
    /* SSE + live WS keep connections open; networkidle never settles. */
  });
}

test.describe("no severe console errors on the paper-trading cockpit", () => {
  test("the /live cockpit renders the trading surface without severe console errors", async ({
    page,
    consoleErrors,
  }) => {
    await page.goto("/live", { waitUntil: "domcontentloaded" });
    await expect(page.getByTestId("app-shell")).toBeVisible();

    // Mounted once either the real cockpit root or the coming-soon placeholder is
    // visible (the paper-trading panels ship after the signal cockpit).
    await expect
      .poll(
        async () => {
          for (const id of ["live-page", "live-placeholder"]) {
            if (await page.getByTestId(id).first().isVisible()) return true;
          }
          return false;
        },
        { timeout: 15_000 },
      )
      .toBe(true);

    // Let the trading panels' polls + the live WS frames flush so any late render
    // error from the blotter/positions/account/reconciliation panels fires.
    await settle(page);
    await page.waitForTimeout(3_000);

    expect(
      consoleErrors,
      `severe console/page errors on the paper cockpit:\n` +
        consoleErrors.map((e) => `  [${e.kind}] ${e.text}`).join("\n"),
    ).toHaveLength(0);
  });

  test("opening the flatten confirmation does not throw a console error", async ({
    page,
    consoleErrors,
  }) => {
    await page.goto("/live", { waitUntil: "domcontentloaded" });
    await expect(page.getByTestId("app-shell")).toBeVisible();

    const realCockpit = page.getByTestId("live-page");
    await expect
      .poll(async () => (await realCockpit.count()) > 0, { timeout: 15_000 })
      .toBeDefined();
    if ((await realCockpit.count()) === 0) {
      test.skip(true, "Live cockpit not yet implemented (coming-soon).");
      return;
    }
    if (!(await liveTradingAvailable())) {
      test.skip(
        true,
        "API started without a trading reader (paper-trading panels absent).",
      );
      return;
    }

    // Open (but DO NOT confirm) the flatten control if present — opening a
    // destructive dialog over live trading state must not throw.
    const flattenButton = page.getByTestId("live-flatten-button");
    if (await flattenButton.count()) {
      await flattenButton.first().click();
      const confirm = page.getByTestId("live-flatten-confirm");
      if (await confirm.count()) {
        await expect(confirm).toBeVisible();
        const cancel = page.getByTestId("live-flatten-confirm-cancel");
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
      `severe console/page errors during a paper-cockpit interaction:\n` +
        consoleErrors.map((e) => `  [${e.kind}] ${e.text}`).join("\n"),
    ).toHaveLength(0);
  });
});
