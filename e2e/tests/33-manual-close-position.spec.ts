/**
 * (Manual desk 2) CLOSE POSITION — click Close on a MANUAL position, confirm, and
 * the position qty drops to 0 with a closing order in the blotter.
 *
 * POST /api/v1/trade/position/{symbol}/close flattens the MANUAL position in one
 * symbol (docs/api.md "Manual trading desk"). A close BYPASSES the budget gate
 * (closes always proceed); a PAPER close still requires the trade-password
 * confirmation (the per-action safety gate; a live close would require the live
 * confirm phrase — never exercised here). Closing an already-flat symbol is an
 * idempotent no-op.
 *
 * This spec requires an OPEN MANUAL position to close. It first ensures one
 * exists (placing a small paper BUY if the book is flat — same gated path as
 * spec 32), then drives the desk's per-position Close control:
 *   - click Close on the position row -> a confirmation appears;
 *   - confirm (the paper trade-password) -> the close order is submitted;
 *   - durable truth: the MANUAL position's signed qty -> 0 (the symbol is flat);
 *   - a closing order appears in the manual blotter.
 *
 * PERMANENT + self-skipping: skips while the desk UI is absent, no desk is
 * connected, the desk is not paper-bound, or no position can be established this
 * run. NEVER closes against a live account.
 *
 * Testid contract:
 *   manual-position-row              — a MANUAL position row; data-symbol /
 *                                      data-signed-qty
 *   manual-position-close            — per-row Close control (scoped to the row)
 *   manual-close-confirm             — the close confirmation dialog
 *   manual-close-confirm-input       — the confirm_token (paper trade password)
 *   manual-close-confirm-submit      — submit (disabled until confirmed)
 *   manual-close-confirm-cancel      — cancel without closing
 */

import { test, expect } from "../fixtures/test";
import {
  withDb,
  latestSession,
  openManualPositions,
  manualPositionSignedQty,
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

/** Place a small paper BUY via the ticket so there is a MANUAL position to close
 * when the book starts flat. Returns the symbol used, or null if the ticket is
 * not surfaced. Mirrors the gated flow proven in spec 32 (never live). */
async function ensureManualPosition(
  page: import("@playwright/test").Page,
  sessionId: number,
): Promise<string | null> {
  const existing = await withDb((c) => openManualPositions(c, sessionId));
  if (existing.length > 0) return existing[0].symbol;

  const ticket = await firstVisibleTestId(page, ["manual-ticket"], 8_000);
  if (!ticket) return null;
  const symbol = process.env.TMS_E2E_MANUAL_SYMBOL?.trim() || "AAPL";
  await page.getByTestId("manual-ticket-symbol").fill(symbol);
  const side = page.getByTestId("manual-ticket-side");
  if (await side.count()) {
    const tag = await side.first().evaluate((el) => el.tagName.toLowerCase());
    if (tag === "select") {
      await side.selectOption("BUY").catch(() => {});
    } else {
      const buy = page.getByTestId("manual-ticket-side-buy");
      if (await buy.count()) await buy.click();
    }
  }
  await page.getByTestId("manual-ticket-qty").fill("10");
  await page.getByTestId("manual-ticket-confirm").fill(PAPER_TRADE_PASSWORD);
  const submit = page.getByTestId("manual-ticket-submit");
  await expect(submit).toBeEnabled({ timeout: 5_000 });
  await submit.click();

  // Wait for the position to open (the mock venue fills, then nets a position).
  await waitFor(
    () => withDb((c) => manualPositionSignedQty(c, sessionId, symbol)),
    (q) => q !== 0,
    { interval: 1_500, timeout: 45_000 },
  );
  return symbol;
}

test.describe("manual desk — close a position", () => {
  test("clicking Close on a MANUAL position confirms and drives the symbol flat", async ({
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
        "manual desk is not paper-bound — refusing to close (never against live).",
      );
      return;
    }

    await expect(page.getByTestId("manual-desk")).toBeVisible();

    const session = await withDb((c) => latestSession(c));
    const sessionId = session!.id;

    const symbol = await ensureManualPosition(page, sessionId);
    if (!symbol) {
      test.skip(
        true,
        "no MANUAL position and no ticket to establish one this run — nothing to close.",
      );
      return;
    }

    // Sanity: the symbol is actually open before we close it.
    const openQtyBefore = await withDb((c) =>
      manualPositionSignedQty(c, sessionId, symbol),
    );
    if (openQtyBefore === 0) {
      test.skip(true, "position did not open this run (no fill) — nothing to close.");
      return;
    }

    const positionsPanel = await firstVisibleTestId(page, ["manual-positions"], 10_000);
    expect(positionsPanel, "the manual positions panel is present").toBeTruthy();

    // Locate the row for our symbol and click its Close control.
    const rowForSymbol = page
      .locator(`[data-testid="manual-position-row"][data-symbol="${symbol}"]`)
      .first();
    await expect(rowForSymbol).toBeVisible({ timeout: 15_000 });

    // The Close control is scoped to the row (per-position close), or a row-level
    // button. Prefer the row-scoped testid; fall back to a global one carrying the
    // symbol.
    const closeInRow = rowForSymbol.getByTestId("manual-position-close");
    const closeBtn = (await closeInRow.count())
      ? closeInRow.first()
      : page
          .locator(`[data-testid="manual-position-close"][data-symbol="${symbol}"]`)
          .first();
    await expect(closeBtn).toBeVisible({ timeout: 10_000 });
    await closeBtn.click();

    // The close confirmation appears; submit is disabled until confirmed (the
    // per-action safety gate — a close still types the paper password).
    const dialog = page.getByTestId("manual-close-confirm");
    await expect(dialog).toBeVisible();
    const submit = page.getByTestId("manual-close-confirm-submit");
    const disabledBefore =
      (await submit.isDisabled().catch(() => false)) ||
      (await submit.getAttribute("aria-disabled")) === "true";
    expect(
      disabledBefore,
      "close submit is disabled until the confirmation is provided",
    ).toBeTruthy();

    await page.getByTestId("manual-close-confirm-input").fill(PAPER_TRADE_PASSWORD);
    await expect(submit).toBeEnabled({ timeout: 5_000 });
    await submit.click();

    // Durable truth: the MANUAL position's signed qty drops to ZERO (flat).
    await expect
      .poll(
        async () => withDb((c) => manualPositionSignedQty(c, sessionId, symbol)),
        { timeout: 45_000 },
      )
      .toBe(0);

    // The positions panel reflects the now-flat symbol — no row reports a non-zero
    // signed qty for it (the panel may keep a closed/flat row).
    await expect
      .poll(
        async () => {
          const rows = page.locator(
            `[data-testid="manual-position-row"][data-symbol="${symbol}"]`,
          );
          const n = await rows.count();
          for (let i = 0; i < n; i++) {
            const sq = await rows.nth(i).getAttribute("data-signed-qty");
            if (sq != null && Number(sq) !== 0) return false;
          }
          return true;
        },
        { timeout: 20_000 },
      )
      .toBe(true);

    // A closing order appears in the manual blotter (the close submitted a real
    // order). The closing order is the opposite side of the prior open.
    const closingSide = openQtyBefore > 0 ? "SELL" : "BUY";
    const blotter = await firstVisibleTestId(page, ["manual-blotter"], 8_000);
    if (blotter) {
      await expect
        .poll(
          async () => {
            const orders = await withDb((c) => recentManualOrders(c, sessionId, 50));
            return orders.some(
              (o) => o.symbol === symbol && o.side === closingSide,
            );
          },
          { timeout: 20_000 },
        )
        .toBe(true);
      const orderRows = page.getByTestId("manual-blotter-order-row");
      await expect
        .poll(async () => orderRows.count(), { timeout: 15_000 })
        .toBeGreaterThan(0);
    }
  });
});
