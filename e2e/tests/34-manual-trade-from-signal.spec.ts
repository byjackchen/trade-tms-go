/**
 * (Manual desk 3) TRADE FROM SIGNAL — click Trade on a watchlist signal and the
 * order ticket pre-fills the symbol (+ side from the signal); submit places the
 * order.
 *
 * The manual desk lets the operator act on a strategy SIGNAL by hand: in signal
 * mode the operator IS the executor (strategies only signal); the Trade affordance
 * on a watchlist/intent row routes that signal into the order ticket pre-filled,
 * then the operator confirms + submits (the same gated paper flow as spec 32). A
 * `buy` intent pre-fills BUY; an `exit`/`stop_hit` intent pre-fills SELL.
 *
 * This spec proves:
 *   - a Trade control exists on a watchlist signal row and opens the ticket;
 *   - the ticket symbol is PRE-FILLED with the signal's symbol (and side is
 *     consistent with the intent state when the desk pre-fills it);
 *   - submitting the pre-filled ticket places a MANUAL order for that symbol
 *     (durable truth: a MANUAL order appears in tms.orders for the symbol).
 *
 * PERMANENT + self-skipping: requires the desk UI + a connected paper desk + at
 * least one watchlist signal to act on. Skips cleanly otherwise; NEVER live.
 *
 * Testid contract:
 *   live-watchlist / live-watchlist-row [data-symbol]   — the watchlist (existing)
 *   manual-trade-from-signal                            — the per-row Trade control
 *     (carries data-symbol / data-side when the signal implies a side)
 *   manual-ticket / manual-ticket-symbol / -side / -qty / -confirm / -submit
 *                                                       — the order ticket (spec 32)
 */

import { test, expect } from "../fixtures/test";
import {
  withDb,
  latestSession,
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

test.describe("manual desk — trade from a watchlist signal", () => {
  test("clicking Trade on a signal pre-fills the ticket with the symbol and side, then submits", async ({
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

    // The Trade-from-signal affordance lives on the watchlist signals. It may be
    // embedded in the desk, or on the existing /live/watchlist view. Find the
    // first Trade control wherever it is surfaced.
    const tradeControl = page.getByTestId("manual-trade-from-signal").first();
    if (!(await tradeControl.count())) {
      // The desk may surface signals only on the dedicated watchlist tab.
      await page.goto("/live/watchlist", { waitUntil: "domcontentloaded" });
      await expect(page.getByTestId("app-shell")).toBeVisible();
    }
    const trade = page.getByTestId("manual-trade-from-signal").first();
    if (!(await trade.count())) {
      test.skip(
        true,
        "no Trade-from-signal control surfaced (no actionable watchlist signal this run).",
      );
      return;
    }
    await expect(trade).toBeVisible({ timeout: 10_000 });

    // Capture the signal's symbol (and side, when the control exposes it) BEFORE
    // clicking, so we can assert the ticket pre-fills from THIS signal.
    const signalSymbol = await trade.getAttribute("data-symbol");
    const signalSide = await trade.getAttribute("data-side");
    expect(signalSymbol, "the Trade control names the signal's symbol").toBeTruthy();

    await trade.click();

    // The order ticket opens, pre-filled with the signal's symbol.
    const ticket = await firstVisibleTestId(page, ["manual-ticket"], 10_000);
    expect(ticket, "the order ticket opens from the Trade affordance").toBeTruthy();

    const symbolInput = page.getByTestId("manual-ticket-symbol");
    await expect
      .poll(async () => (await symbolInput.inputValue()).trim(), { timeout: 10_000 })
      .toBe(signalSymbol);

    // When the signal implies a side, the ticket pre-fills it.
    if (signalSide) {
      const sideControl = page.getByTestId("manual-ticket-side");
      if (await sideControl.count()) {
        const tag = await sideControl
          .first()
          .evaluate((el) => el.tagName.toLowerCase());
        if (tag === "select" || tag === "input") {
          await expect
            .poll(
              async () =>
                (await sideControl.first().inputValue().catch(() => "")).toUpperCase(),
              { timeout: 5_000 },
            )
            .toBe(signalSide.toUpperCase());
        } else {
          // A toggle/segmented control marks the active side via data-active.
          const active = page.locator(
            `[data-testid="manual-ticket-side"] [data-active="true"], [data-testid^="manual-ticket-side-"][data-active="true"]`,
          );
          if (await active.count()) {
            const txt = ((await active.first().getAttribute("data-side")) ??
              (await active.first().innerText())).toUpperCase();
            expect(txt).toContain(signalSide.toUpperCase());
          }
        }
      }
    }

    // Provide a qty if the ticket did not pre-fill one, then confirm + submit.
    const qtyInput = page.getByTestId("manual-ticket-qty");
    const qtyVal = (await qtyInput.inputValue()).trim();
    if (qtyVal === "" || Number(qtyVal) === 0) {
      await qtyInput.fill("10");
    }
    await page.getByTestId("manual-ticket-confirm").fill(PAPER_TRADE_PASSWORD);
    const submit = page.getByTestId("manual-ticket-submit");
    await expect(submit).toBeEnabled({ timeout: 5_000 });
    await submit.click();

    // Durable truth: a MANUAL order for the signal's symbol was placed.
    const placed = await waitFor(
      () => withDb((c) => recentManualOrders(c, sessionId, 50)),
      (os) => os.some((o) => o.symbol === signalSymbol),
      { interval: 1_000, timeout: 25_000 },
    );
    expect(
      placed.some((o) => o.symbol === signalSymbol),
      "submitting the pre-filled ticket placed a MANUAL order for the signal's symbol",
    ).toBeTruthy();

    // When the signal carried a side, the placed order honours it.
    if (signalSide) {
      const mine = placed.find((o) => o.symbol === signalSymbol);
      if (mine) {
        expect(
          mine.side,
          "the placed order's side matches the signal's pre-filled side",
        ).toBe(signalSide.toUpperCase());
      }
    }
  });
});
