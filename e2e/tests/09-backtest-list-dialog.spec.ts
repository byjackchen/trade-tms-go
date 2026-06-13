/**
 * (4) Backtests list + New-backtest dialog UI contract.
 *
 * Unlike the launch/detail flow specs (07/08) — which need a worker and tradable
 * bars — this spec exercises only the list page and the dialog form, so it runs
 * on any stack where the Backtests workspace is wired (it self-skips while the
 * section is still the coming-soon placeholder).
 *
 * It asserts:
 *   - the runs list mounts (table or a documented empty-state) and the status
 *     filter is present;
 *   - the New-backtest dialog opens with its form, validates client-side
 *     (bad date, end-before-start, empty tickers), and the cancel button closes
 *     it without enqueuing anything;
 *   - switching the instrument source to "universe" hides the intents editor.
 *
 * No job is enqueued, so this never mutates the stack.
 */

import { test, expect } from "../fixtures/test";

async function backtestsUiReady(
  page: import("@playwright/test").Page,
): Promise<boolean> {
  await page.goto("/backtests", { waitUntil: "domcontentloaded" });
  await expect(page.getByTestId("app-shell")).toBeVisible();
  const placeholder = page.getByTestId("backtests-placeholder");
  const launcher = page.getByTestId("open-backtest-dialog");
  await expect
    .poll(async () => (await placeholder.count()) + (await launcher.count()), {
      timeout: 15_000,
    })
    .toBeGreaterThan(0);
  return (await launcher.count()) > 0;
}

test.describe("backtests list + dialog UI", () => {
  test("runs list mounts with a status filter", async ({ page }) => {
    if (!(await backtestsUiReady(page))) {
      test.skip(true, "Backtests workspace not yet implemented (coming-soon).");
      return;
    }

    await expect(page.getByTestId("backtests-page")).toBeVisible();
    await expect(page.getByTestId("runs-card")).toBeVisible();
    await expect(page.getByTestId("runs-status-filter")).toBeVisible();

    // The runs surface resolves to exactly one of: the table, the empty-state,
    // or an error-state — never an indefinite spinner.
    await expect
      .poll(
        async () =>
          (await page.getByTestId("runs-table").count()) +
          (await page.getByTestId("runs-empty").count()) +
          (await page.getByTestId("runs-error").count()),
        { timeout: 20_000 },
      )
      .toBeGreaterThan(0);
  });

  test("the New-backtest dialog opens, validates, and cancels cleanly", async ({
    page,
  }) => {
    if (!(await backtestsUiReady(page))) {
      test.skip(true, "Backtests workspace not yet implemented (coming-soon).");
      return;
    }

    // Open.
    await page.getByTestId("open-backtest-dialog").click();
    const dialog = page.getByTestId("backtest-dialog");
    await expect(dialog).toBeVisible();
    const form = page.getByTestId("backtest-form");
    await expect(form).toBeVisible();

    // Client-side validation: a malformed start date is rejected without a
    // network round-trip (the job-progress panel never appears).
    await page.getByTestId("backtest-start").fill("not-a-date");
    await page.getByTestId("backtest-end").fill("2024-12-31");
    await page.getByTestId("backtest-submit").click();
    await expect(page.getByTestId("new-backtest-error")).toBeVisible();
    await expect(page.getByTestId("job-progress")).toHaveCount(0);

    // end-before-start is rejected too.
    await page.getByTestId("backtest-start").fill("2024-06-01");
    await page.getByTestId("backtest-end").fill("2024-01-01");
    await page.getByTestId("backtest-submit").click();
    await expect(page.getByTestId("new-backtest-error")).toBeVisible();
    await expect(page.getByTestId("job-progress")).toHaveCount(0);

    // Empty tickers (explicit mode) is rejected.
    await page.getByTestId("backtest-start").fill("2024-01-02");
    await page.getByTestId("backtest-end").fill("2024-12-31");
    await page.getByTestId("backtest-tickers").fill("");
    await page.getByTestId("backtest-submit").click();
    await expect(page.getByTestId("new-backtest-error")).toBeVisible();
    await expect(page.getByTestId("job-progress")).toHaveCount(0);

    // Switching to the universe source hides the explicit intents editor.
    await page.getByTestId("backtest-source").selectOption("universe");
    await expect(page.getByTestId("backtest-intents")).toHaveCount(0);
    await expect(page.getByTestId("backtest-universe-table")).toBeVisible();

    // Cancel closes the dialog; nothing was enqueued.
    await page.getByTestId("backtest-cancel").click();
    await expect(dialog).toBeHidden();
  });

  test("the realistic fill profile reveals a slippage input", async ({
    page,
  }) => {
    if (!(await backtestsUiReady(page))) {
      test.skip(true, "Backtests workspace not yet implemented (coming-soon).");
      return;
    }

    await page.getByTestId("open-backtest-dialog").click();
    await expect(page.getByTestId("backtest-form")).toBeVisible();

    // Default is nautilus-compat → no slippage field.
    await expect(page.getByTestId("backtest-slippage")).toHaveCount(0);

    await page.getByTestId("backtest-fill-profile").selectOption("realistic");
    await expect(page.getByTestId("backtest-slippage")).toBeVisible();

    await page.getByTestId("backtest-cancel").click();
  });
});
