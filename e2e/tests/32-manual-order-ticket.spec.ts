/**
 * (Manual desk 1) ORDER TICKET — place a manual BUY against the PAPER mock venue
 * and prove it flows submitted -> filled, opens a position, updates the account,
 * and that the UI matches tms.orders / fills / positions in the DB.
 *
 * The MANUAL desk is the ONLY broker-mutation surface in the HTTP API (docs/api.md
 * "Manual trading desk"): the operator places an order BY HAND, attributed to the
 * `MANUAL` pseudo-strategy, reusing the MoomooExecutor + order-state machine + the
 * durable tms.orders/fills/positions + the mock trading venue (which simulates
 * accept->fill). The desk runs in PAPER in the gate — a paper manual order
 * requires the trade password in `confirm_token`; there is NO real account here.
 *
 * This spec proves, end to end through the desk:
 *   - the order TICKET places a BUY (symbol + qty + the paper trade-password
 *     confirmation), and the API returns a deterministic client_order_id
 *     (idempotency_key -> client-order-id, no double-submit);
 *   - the BLOTTER shows that order reach terminal FILLED (submitted -> filled);
 *   - the POSITIONS panel gains the symbol under the MANUAL book;
 *   - the ACCOUNT / day-P&L card renders;
 *   - and — the whole point — every rendered row MATCHES the DB (the UI is a
 *     faithful proxy of the durable truth, never a fabricated row).
 *
 * PERMANENT + self-skipping: while the manual desk UI is not built, no desk is
 * connected (`/api/v1/trade/*` 503), or the desk is not paper-bound, it self-skips
 * cleanly so the gate stays green — exactly like specs 24-30 do for the auto book.
 * It NEVER places against a live account (gated by manualDeskIsPaper).
 *
 * Testid contract (the manual desk implements it):
 *   manual-desk                  — the manual-desk root
 *   manual-ticket                — the order-ticket panel
 *   manual-ticket-symbol         — the symbol input
 *   manual-ticket-side           — the BUY/SELL control (BUY|SELL values)
 *   manual-ticket-qty            — the quantity input
 *   manual-ticket-confirm        — the confirm_token input (paper trade password)
 *   manual-ticket-submit         — submit (disabled until required fields + confirm)
 *   manual-blotter               — the manual order blotter
 *   manual-blotter-order-row     — one row per manual order; data-client-order-id /
 *                                  data-symbol / data-status / data-filled-qty
 *   manual-positions             — the manual positions panel
 *   manual-position-row          — one row per open MANUAL position; data-symbol /
 *                                  data-signed-qty
 *   manual-account               — the account snapshot panel
 *   manual-account-day-pnl       — the day-P&L card; data-day-pnl-usd
 */

import { test, expect } from "../fixtures/test";
import {
  withDb,
  latestSession,
  recentManualOrders,
  manualOrderByClientId,
  openManualPositions,
  manualAuditCount,
  sessionDayPnlUsd,
} from "../lib/db";
import {
  manualDeskUiReady,
  manualDeskAvailable,
  manualDeskIsPaper,
  firstVisibleTestId,
  waitFor,
  MANUAL_STRATEGY_ID,
} from "../lib/live";

/** The paper trade-password phrase the desk demands in confirm_token for a paper
 * manual order (docs/api.md: TMS_MOOMOO_TRADE_PASSWORD). The gate seeds a known
 * value; override via TMS_E2E_PAPER_TRADE_PASSWORD if the stack uses another. */
const PAPER_TRADE_PASSWORD =
  process.env.TMS_E2E_PAPER_TRADE_PASSWORD?.trim() || "paper-trade-password";

test.describe("manual desk — order ticket places a paper BUY that fills", () => {
  test("a manual BUY flows submitted->filled, opens a position, and the UI matches the DB", async ({
    page,
  }) => {
    if (!(await manualDeskUiReady(page))) {
      test.skip(true, "Manual trading desk not yet implemented (coming-soon).");
      return;
    }
    if (!(await manualDeskAvailable())) {
      test.skip(
        true,
        "no manual desk connected (POST /api/v1/trade/* 503) — desk not attached.",
      );
      return;
    }
    // SAFETY: only ever place against a PAPER (or signal/mock) desk — never live.
    if (!(await manualDeskIsPaper())) {
      test.skip(
        true,
        "manual desk is not paper-bound — refusing to place an order (never against live).",
      );
      return;
    }

    await expect(page.getByTestId("manual-desk")).toBeVisible();

    const session = await withDb((c) => latestSession(c));
    expect(session, "a trading session exists").not.toBeNull();
    const sessionId = session!.id;

    const ticket = await firstVisibleTestId(page, ["manual-ticket"], 10_000);
    if (!ticket) {
      test.skip(true, "order-ticket panel not surfaced yet.");
      return;
    }

    // Audit is monotonic: every manual action writes an ops.audit_log row.
    const auditBefore = await withDb((c) => manualAuditCount(c));

    // ----- FILL OUT + SUBMIT THE TICKET --------------------------------------
    // A liquid symbol the mock venue prices. AAPL is in the docs example body.
    const symbol = process.env.TMS_E2E_MANUAL_SYMBOL?.trim() || "AAPL";
    const qty = 10;

    await page.getByTestId("manual-ticket-symbol").fill(symbol);

    // Side defaults to BUY; set it explicitly when the control is a select.
    const side = page.getByTestId("manual-ticket-side");
    if (await side.count()) {
      const tag = await side.first().evaluate((el) => el.tagName.toLowerCase());
      if (tag === "select") {
        await side.selectOption("BUY").catch(() => {
          /* some builds use a toggle; BUY is the default */
        });
      } else {
        // A toggle/segmented control: click the BUY affordance if distinct.
        const buy = page.getByTestId("manual-ticket-side-buy");
        if (await buy.count()) await buy.click();
      }
    }

    await page.getByTestId("manual-ticket-qty").fill(String(qty));

    // The paper desk REQUIRES the trade password in confirm_token — submit must
    // be disabled until it is provided (the per-order safety gate). Assert the
    // guard, then provide the password.
    const submit = page.getByTestId("manual-ticket-submit");
    const disabledBeforeConfirm =
      (await submit.isDisabled().catch(() => false)) ||
      (await submit.getAttribute("aria-disabled")) === "true";
    expect(
      disabledBeforeConfirm,
      "ticket submit is disabled until the paper trade-password confirmation is typed",
    ).toBeTruthy();

    await page.getByTestId("manual-ticket-confirm").fill(PAPER_TRADE_PASSWORD);
    await expect(submit).toBeEnabled({ timeout: 5_000 });
    await submit.click();

    // ----- BLOTTER: the new manual order appears and reaches FILLED ----------
    const blotter = await firstVisibleTestId(page, ["manual-blotter"], 10_000);
    expect(blotter, "the manual blotter renders after a placement").toBeTruthy();

    // Durable truth: a MANUAL order was persisted for this session.
    const placed = await waitFor(
      () => withDb((c) => recentManualOrders(c, sessionId, 50)),
      (os) => os.length > 0,
      { interval: 1_000, timeout: 20_000 },
    );
    expect(
      placed.length,
      "the manual BUY was persisted as a MANUAL order in tms.orders",
    ).toBeGreaterThan(0);

    // The newest MANUAL order for our symbol is the one we just placed.
    const mine = placed.find((o) => o.symbol === symbol) ?? placed[0];
    expect(mine.strategyId, "manual order is booked under the MANUAL pseudo-strategy").toBe(
      MANUAL_STRATEGY_ID,
    );
    expect(mine.side, "the placed order is a BUY").toBe("BUY");
    expect(mine.symbol, "the placed order carries the ticket symbol").toBe(symbol);
    const coid = mine.clientOrderId;
    expect(coid, "the order has a deterministic client-order-id").toBeTruthy();

    // The mock venue simulates accept->fill; wait (bounded) for terminal FILLED.
    const filled = await waitFor(
      () => withDb((c) => manualOrderByClientId(c, sessionId, coid)),
      (o) => o?.status === "FILLED",
      { interval: 1_500, timeout: 45_000 },
    );
    expect(
      filled?.status,
      "the manual BUY reached terminal FILLED on the mock venue (submitted -> filled)",
    ).toBe("FILLED");

    // The blotter row for our order MATCHES the DB (faithful proxy, never fabricated).
    const rows = page.getByTestId("manual-blotter-order-row");
    await expect
      .poll(async () => rows.count(), { timeout: 20_000 })
      .toBeGreaterThan(0);
    // Wait for the UI to converge to the durable FILLED status on our row.
    await expect
      .poll(
        async () => {
          const n = await rows.count();
          for (let i = 0; i < n; i++) {
            const r = rows.nth(i);
            if ((await r.getAttribute("data-client-order-id")) !== coid) continue;
            return (await r.getAttribute("data-status"))?.toUpperCase() ?? null;
          }
          return null;
        },
        { timeout: 20_000 },
      )
      .toBe("FILLED");
    // The matched row's symbol agrees with the DB order.
    {
      const n = await rows.count();
      let matched = false;
      for (let i = 0; i < n; i++) {
        const r = rows.nth(i);
        if ((await r.getAttribute("data-client-order-id")) !== coid) continue;
        matched = true;
        const sym = await r.getAttribute("data-symbol");
        if (sym != null) expect(sym, "blotter row symbol matches the DB").toBe(symbol);
      }
      expect(matched, "the placed order's client-order-id is in the blotter").toBeTruthy();
    }

    // ----- POSITIONS: the MANUAL book gains the symbol -----------------------
    const dbPositions = await waitFor(
      () => withDb((c) => openManualPositions(c, sessionId)),
      (ps) => ps.some((p) => p.symbol === symbol),
      { interval: 1_500, timeout: 30_000 },
    );
    expect(
      dbPositions.some((p) => p.symbol === symbol),
      "the MANUAL position book gained the symbol after the fill",
    ).toBeTruthy();

    const positionsPanel = await firstVisibleTestId(page, ["manual-positions"], 8_000);
    if (positionsPanel) {
      const posRows = page.getByTestId("manual-position-row");
      const dbSymbols = new Set(dbPositions.map((p) => p.symbol));
      // The panel renders the symbol; every rendered row is a real MANUAL position.
      await expect
        .poll(
          async () => {
            const n = await posRows.count();
            for (let i = 0; i < n; i++) {
              if ((await posRows.nth(i).getAttribute("data-symbol")) === symbol) {
                return true;
              }
            }
            return false;
          },
          { timeout: 15_000 },
        )
        .toBe(true);
      const pn = await posRows.count();
      for (let i = 0; i < pn; i++) {
        const sym = await posRows.nth(i).getAttribute("data-symbol");
        if (sym != null) {
          expect(
            dbSymbols.has(sym),
            `manual position row ${sym} is a real open MANUAL position in the DB`,
          ).toBeTruthy();
        }
      }
    }

    // ----- ACCOUNT (day P&L) -------------------------------------------------
    const accountPanel = await firstVisibleTestId(page, ["manual-account"], 8_000);
    if (accountPanel) {
      const dayPnlCard = page.getByTestId("manual-account-day-pnl");
      await expect(dayPnlCard).toBeVisible({ timeout: 10_000 });
      const dbDayPnl = await withDb((c) => sessionDayPnlUsd(c, sessionId));
      const attr = await dayPnlCard.getAttribute("data-day-pnl-usd");
      if (attr != null) {
        const rendered = Number(attr);
        expect(Number.isFinite(rendered), "day-pnl attr is numeric").toBeTruthy();
        expect(
          Math.abs(rendered - dbDayPnl),
          `account day-P&L card matches Σ realized in the DB (got ${rendered}, db ${dbDayPnl})`,
        ).toBeLessThanOrEqual(0.01);
      }
    }

    // ----- AUDIT: the placement was audited ----------------------------------
    const auditAfter = await waitFor(
      () => withDb((c) => manualAuditCount(c)),
      (n) => n > auditBefore,
      { interval: 1_000, timeout: 15_000 },
    );
    expect(
      auditAfter,
      "the manual placement wrote at least one ops.audit_log row",
    ).toBeGreaterThan(auditBefore);
  });
});
