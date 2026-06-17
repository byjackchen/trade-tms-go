/**
 * (Manual desk 6) CANCEL a working order + ZERO severe console errors on the
 * trade surface.
 *
 * POST /api/v1/trade/order/{coid}/cancel cancels a working manual order by its
 * client-order-id (docs/api.md "Manual trading desk"). It is idempotent (cancelling
 * an unknown / already-terminal order is a no-op success) and confirms via the
 * normal order-update push (`CANCELLED_ALL`). A wire build without the modify-order
 * proto returns 501 `cancel_unsupported` rather than ever falsely telling the
 * operator a working REAL order was cancelled.
 *
 * Two parts:
 *   (1) CANCEL: place a LIMIT order far from the market (so it rests WORKING on the
 *       mock venue, not instantly filled), then click Cancel on its blotter row;
 *       the order reaches a terminal cancelled state (CANCELLED / CANCELLED_ALL) —
 *       OR, if the wire build lacks modify, the cancel surfaces a 501
 *       cancel_unsupported (the safe truthful path). Either is a correct outcome.
 *   (2) CONSOLE: the trade surface (ticket + blotter + positions + account, and
 *       opening — never confirming — the live-arm dialog) produces ZERO severe
 *       console / page errors.
 *
 * PERMANENT + self-skipping; paper only; never live.
 *
 * Testid contract:
 *   manual-ticket-type               — order type (MARKET|LIMIT)
 *   manual-ticket-limit-price        — the limit price input (LIMIT only)
 *   manual-blotter-order-row         — a blotter row; data-client-order-id /
 *                                      data-status
 *   manual-order-cancel              — per-row Cancel control (scoped to the row)
 */

import { test, expect } from "../fixtures/test";
import {
  withDb,
  latestSession,
  manualOrderByClientId,
  recentManualOrders,
} from "../lib/db";
import {
  manualDeskUiReady,
  manualDeskAvailable,
  manualDeskIsPaper,
  firstVisibleTestId,
  waitFor,
} from "../lib/live";

const PAPER_TRADE_PASSWORD =
  process.env.TMS_E2E_PAPER_TRADE_PASSWORD?.trim() || "paper-trade-password";

/** Terminal states a successfully-cancelled order may land in. */
const CANCELLED_STATES = new Set(["CANCELLED", "CANCELLED_ALL", "CANCELED"]);

async function settle(page: import("@playwright/test").Page): Promise<void> {
  await page.waitForLoadState("networkidle", { timeout: 2_000 }).catch(() => {
    /* SSE + live WS keep connections open; networkidle never settles. */
  });
}

test.describe("manual desk — cancel a working order", () => {
  test("a resting LIMIT order can be cancelled (or truthfully reports cancel_unsupported)", async ({
    page,
  }) => {
    if (!(await manualDeskUiReady(page))) {
      test.skip(true, "Manual trading desk not yet implemented (coming-soon).");
      return;
    }
    if (!(await manualDeskAvailable())) {
      test.skip(true, "no manual desk connected (POST /api/v1/trade/* 503).");
      return;
    }
    if (!(await manualDeskIsPaper())) {
      test.skip(
        true,
        "manual desk is not paper-bound — refusing to place (never against live).",
      );
      return;
    }

    await expect(page.getByTestId("manual-desk")).toBeVisible();
    const session = await withDb((c) => latestSession(c));
    const sessionId = session!.id;
    const symbol = process.env.TMS_E2E_MANUAL_SYMBOL?.trim() || "AAPL";

    const ticket = await firstVisibleTestId(page, ["manual-ticket"], 10_000);
    if (!ticket) {
      test.skip(true, "order-ticket panel not surfaced yet.");
      return;
    }
    const typeControl = page.getByTestId("manual-ticket-type");
    if (!(await typeControl.count())) {
      test.skip(
        true,
        "ticket has no LIMIT type control — cannot rest a working order to cancel.",
      );
      return;
    }

    // Place a BUY LIMIT far BELOW the market so it rests WORKING (never fills) on
    // the mock venue — giving us a live order to cancel.
    await page.getByTestId("manual-ticket-symbol").fill(symbol);
    const sideSel = page.getByTestId("manual-ticket-side");
    if (await sideSel.count()) {
      const tag = await sideSel.first().evaluate((el) => el.tagName.toLowerCase());
      if (tag === "select") await sideSel.selectOption("BUY").catch(() => {});
    }
    await page.getByTestId("manual-ticket-qty").fill("10");
    const tTag = await typeControl.first().evaluate((el) => el.tagName.toLowerCase());
    if (tTag === "select") {
      await typeControl.selectOption("LIMIT").catch(() => {});
    } else {
      const limitToggle = page.getByTestId("manual-ticket-type-limit");
      if (await limitToggle.count()) await limitToggle.click();
    }
    const limitPrice = page.getByTestId("manual-ticket-limit-price");
    await expect(limitPrice).toBeVisible({ timeout: 5_000 });
    // A deliberately low limit so a BUY never crosses (rests working).
    await limitPrice.fill("0.01");
    await page.getByTestId("manual-ticket-confirm").fill(PAPER_TRADE_PASSWORD);
    const submit = page.getByTestId("manual-ticket-submit");
    await expect(submit).toBeEnabled({ timeout: 5_000 });
    await submit.click();

    // Find the just-placed working LIMIT order (newest LIMIT-ish MANUAL order for
    // the symbol that is NOT terminal-filled).
    const placed = await waitFor(
      () => withDb((c) => recentManualOrders(c, sessionId, 50)),
      (os) => os.some((o) => o.symbol === symbol),
      { interval: 1_000, timeout: 20_000 },
    );
    const working = placed.find(
      (o) => o.symbol === symbol && o.status !== "FILLED" && o.status !== "REJECTED",
    );
    if (!working) {
      test.skip(
        true,
        "the LIMIT order did not rest working this run (filled/rejected immediately) — nothing to cancel.",
      );
      return;
    }
    const coid = working.clientOrderId;

    // Click Cancel on its blotter row.
    const row = page
      .locator(`[data-testid="manual-blotter-order-row"][data-client-order-id="${coid}"]`)
      .first();
    await expect(row).toBeVisible({ timeout: 15_000 });
    const cancelInRow = row.getByTestId("manual-order-cancel");
    const cancelBtn = (await cancelInRow.count())
      ? cancelInRow.first()
      : page
          .locator(`[data-testid="manual-order-cancel"][data-client-order-id="${coid}"]`)
          .first();
    await expect(cancelBtn).toBeVisible({ timeout: 10_000 });
    await cancelBtn.click();

    // Outcome: EITHER the order reaches a terminal cancelled state in the DB, OR
    // the desk truthfully surfaces 501 cancel_unsupported (a wire build without
    // modify — the operator is never falsely told a real order was cancelled).
    const unsupported = page.getByTestId("manual-cancel-unsupported");
    const reachedTerminal = await waitFor(
      () => withDb((c) => manualOrderByClientId(c, sessionId, coid)),
      (o) => !!o && CANCELLED_STATES.has(o.status.toUpperCase()),
      { interval: 1_500, timeout: 30_000 },
    );
    const cancelled =
      !!reachedTerminal && CANCELLED_STATES.has(reachedTerminal.status.toUpperCase());
    const unsupportedShown =
      (await unsupported.count()) > 0 &&
      (await unsupported.isVisible().catch(() => false));
    expect(
      cancelled || unsupportedShown,
      "the cancel either drove the order terminal-cancelled or truthfully reported cancel_unsupported",
    ).toBeTruthy();

    if (cancelled) {
      // The blotter row converges to the cancelled status (faithful proxy).
      await expect
        .poll(
          async () => {
            const r = page
              .locator(
                `[data-testid="manual-blotter-order-row"][data-client-order-id="${coid}"]`,
              )
              .first();
            return ((await r.getAttribute("data-status")) ?? "").toUpperCase();
          },
          { timeout: 20_000 },
        )
        .toMatch(/CANCEL/);
    }
  });

  test("the manual trade surface renders with zero severe console errors", async ({
    page,
    consoleErrors,
  }) => {
    // In the FINAL 4-top IA the desk is the Desk view of the unified trade module
    // at /trade?view=desk (the old /trade/desk + /paper?view=desk routes 301 here).
    await page.goto("/trade?view=desk", { waitUntil: "domcontentloaded" });
    await expect(page.getByTestId("app-shell")).toBeVisible();

    // Mounted once either the real desk root or the coming-soon placeholder shows.
    await expect
      .poll(
        async () => {
          for (const id of ["manual-desk", "manual-desk-placeholder"]) {
            if (await page.getByTestId(id).first().isVisible()) return true;
          }
          return false;
        },
        { timeout: 15_000 },
      )
      .toBe(true);

    // Let the desk's panels (ticket / blotter / positions / account) + the live WS
    // frames flush so any late render error fires.
    await settle(page);
    await page.waitForTimeout(2_500);

    // Open (but never confirm) the live-arm dialog if present — opening a
    // dangerous dialog over live trading state must not throw.
    const realDesk = page.getByTestId("manual-desk");
    if (
      (await realDesk.count()) &&
      (await manualDeskAvailable().catch(() => false))
    ) {
      const liveSwitch = page.getByTestId("manual-mode-live");
      if (await liveSwitch.count()) {
        await liveSwitch.first().click();
        const dialog = page.getByTestId("manual-live-confirm");
        if (await dialog.count()) {
          await expect(dialog).toBeVisible();
          const cancel = page.getByTestId("manual-live-confirm-cancel");
          if (await cancel.count()) {
            await cancel.click();
          } else {
            await page.keyboard.press("Escape");
          }
        }
      }
    }

    await settle(page);
    await page.waitForTimeout(1_500);
    expect(
      consoleErrors,
      `severe console/page errors on the manual trade surface:\n` +
        consoleErrors.map((e) => `  [${e.kind}] ${e.text}`).join("\n"),
    ).toHaveLength(0);
  });
});
