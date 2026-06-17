/**
 * (Manual desk 5) LIVE SAFETY — a real-money manual order is impossible without
 * the full gate; a paper/signal desk can NEVER reach a live account.
 *
 * POST-RESTRUCTURE: the manual order body no longer carries mode/live routing
 * hints — the desk is bound paper/live at CONNECT, and the handler ignores any
 * such hints (internal/api/handlers_manual_trade.go). The per-order gate is
 * `confirm_token` (the live confirm phrase on a live desk; the trade password on a
 * paper desk). This spec's invariants are reframed onto that contract; the safety
 * status codes (412) are unchanged.
 *
 * This is the TOP acceptance criterion (SAFE): the manual desk can place REAL
 * orders. P6 + docs/api.md "Manual trading desk" require a LIVE manual order to
 * carry the FULL 4-factor live activation (real acc id + TMS_LIVE_CONFIRM phrase +
 * UnlockTrade + the TMS-LIVE-REAL-001 trader id — proven by the desk's live-bound
 * executor) PLUS a per-order typed confirmation phrase in `confirm_token`
 * (`I CONFIRM THIS REAL MONEY MANUAL ORDER`). Missing/wrong ⇒ 412
 * `confirmation_required` and NO order is placed. There must be NO path to a real
 * order without the full gate, and a paper/signal desk must never reach the live
 * account.
 *
 * This spec asserts the GUARDS EXIST without ever placing a real order (there is
 * no real account in the gate — the desk is paper/mock):
 *   (a) API: a manual order WITHOUT the per-order confirm_token (the live confirm
 *       phrase for a live desk, the trade password for a paper desk) is rejected
 *       412 `confirmation_required`. The SAME-shaped request with a WRONG
 *       confirm_token is ALSO 412 (a near-miss never arms a real order). No live
 *       order is placed (no FILLED MANUAL live order appears).
 *   (b) API: a paper/signal desk cannot reach a live account — handing it the LIVE
 *       confirm phrase as confirm_token does NOT place an order (the desk's
 *       CONNECT-time binding is paper, the live phrase is not the paper password),
 *       refused (412/422/400/403), never silently accepted as a real order.
 *   (c) UI: the desk's switch-to-live (if present) opens the guarded confirm
 *       dialog whose submit is DISABLED until the EXACT phrase is typed; a
 *       wrong/near-miss phrase never arms it; we CANCEL — never completing a live
 *       switch. No direct-to-live "place live order" affordance exists.
 *
 * Runs whenever a manual desk is connected; the API guards are always safe (a 412
 * means live did NOT activate / no real order was placed).
 *
 * Testid contract:
 *   manual-desk                       — the desk root
 *   manual-mode-live                  — (optional) the desk's switch-to-live control
 *   manual-live-confirm               — the live-arming confirmation dialog
 *   manual-live-confirm-phrase        — the typed phrase input
 *   manual-live-confirm-submit        — submit (disabled until the EXACT phrase)
 *   manual-live-confirm-cancel        — cancel without arming live
 */

import { test, expect } from "../fixtures/test";
import { postManual } from "../lib/api";
import { withDb, latestSession, recentManualOrders } from "../lib/db";
import {
  manualDeskUiReady,
  manualDeskAvailable,
  MANUAL_LIVE_CONFIRM_PHRASE,
} from "../lib/live";

/** A status that proves the boundary REFUSED a live order (never a 200 success).
 * 412 is the documented confirmation_required; 422/400/403 are also refusals (the
 * point is: a real order was NOT placed). A 503 (no desk) is handled by skip. */
function isRefusal(status: number): boolean {
  return status === 412 || status === 422 || status === 400 || status === 403;
}

test.describe("manual desk — LIVE safety (no real order without the full gate)", () => {
  test("the API rejects a live manual order without the per-order confirm phrase (412)", async () => {
    if (!(await manualDeskAvailable())) {
      test.skip(true, "no manual desk connected (POST /api/v1/trade/* 503).");
      return;
    }

    const session = await withDb((c) => latestSession(c));
    const sessionId = session?.id ?? null;
    const symbol = process.env.TMS_E2E_MANUAL_SYMBOL?.trim() || "AAPL";

    // POST-RESTRUCTURE: the desk is bound paper/live at CONNECT, so the order body
    // no longer carries mode/live routing hints (the handler ignores them —
    // handlers_manual_trade.go: "request-level routing hints are NOT honored").
    // The per-order gate is `confirm_token`: a live desk requires the live confirm
    // phrase; a paper desk requires the trade password. EITHER way, an order
    // WITHOUT a confirm_token is refused 412 confirmation_required — no order
    // reaches the venue. (The gate desk is paper, so this is the trade-password
    // gate, also mapped to confirmation_required.)
    const noPhrase = await postManual("trade/order", {
      idempotency_key: `e2e-livesafe-nophrase-${Date.now()}`,
      symbol,
      side: "BUY",
      qty: 1,
      type: "MARKET",
      override: false,
      reason: "e2e live-safety guard (no confirm phrase) — expect 412",
    });
    expect(
      noPhrase.status,
      "a live manual order WITHOUT the confirm phrase is a confirmation-required 412",
    ).toBe(412);
    const nb = noPhrase.body as
      | { error?: { code?: string }; code?: string }
      | undefined;
    expect(nb?.error?.code ?? nb?.code).toBe("confirmation_required");

    // The SAME request with a WRONG confirm_token is ALSO refused 412 — a near-miss
    // never arms an order (the exact phrase/password is required; this lowercased
    // value matches neither the live phrase nor the paper password).
    const wrongPhrase = await postManual("trade/order", {
      idempotency_key: `e2e-livesafe-wrong-${Date.now()}`,
      symbol,
      side: "BUY",
      qty: 1,
      type: "MARKET",
      override: false,
      confirm_token: "i confirm this real money manual order", // wrong case/exactness
      reason: "e2e live-safety guard (wrong phrase) — expect 412",
    });
    expect(
      wrongPhrase.status,
      "a live manual order with a WRONG confirm phrase is still 412 (near-miss never arms)",
    ).toBe(412);

    // Durable: NO live MANUAL order was placed by either rejected call. There is
    // no FILLED MANUAL live order — the boundary held. (We check the session's
    // MANUAL book carries no order whose reason marks these e2e live attempts.)
    if (sessionId != null) {
      const manual = await withDb((c) => recentManualOrders(c, sessionId, 200));
      const leaked = manual.filter(
        (o) =>
          (o.reason ?? "").includes("e2e live-safety guard") &&
          o.status !== "REJECTED",
      );
      expect(
        leaked.length,
        "no real order was placed by the rejected live attempts (the gate held)",
      ).toBe(0);
    }
  });

  test("a paper/signal desk cannot target a live account", async () => {
    if (!(await manualDeskAvailable())) {
      test.skip(true, "no manual desk connected (POST /api/v1/trade/* 503).");
      return;
    }

    const session = await withDb((c) => latestSession(c));
    // This invariant is about a NON-live desk reaching live. If the desk/session
    // is already live (never in the gate), this case does not apply.
    if (session?.mode === "live") {
      test.skip(true, "session already live — not the paper/signal -> live case.");
      return;
    }
    const symbol = process.env.TMS_E2E_MANUAL_SYMBOL?.trim() || "AAPL";

    // POST-RESTRUCTURE: there is NO per-order "route to live" flag — the desk's
    // CONNECT-time binding alone determines the account. So the proof that a
    // paper/signal desk cannot reach live is that handing it the LIVE confirm
    // phrase as confirm_token does NOT place an order: on a paper desk the live
    // phrase is not the paper trade password, so the gate refuses it (412
    // confirmation_required). The one and only path to live is re-binding the desk
    // live (the guarded UI switch), never an inline per-order token from a paper
    // desk.
    const res = await postManual("trade/order", {
      idempotency_key: `e2e-paper-to-live-${Date.now()}`,
      symbol,
      side: "BUY",
      qty: 1,
      type: "MARKET",
      override: false,
      confirm_token: MANUAL_LIVE_CONFIRM_PHRASE,
      reason: "e2e paper-desk -> live account — must be refused",
    });
    expect(
      isRefusal(res.status),
      `a paper/signal desk targeting live is refused (got ${res.status}, must not be a 200 real order)`,
    ).toBeTruthy();
    expect(
      res.status,
      "a paper/signal desk NEVER gets a 200 success routing to a live account",
    ).not.toBe(200);
  });

  test("the desk switch-to-live opens a guarded confirm dialog and never auto-arms", async ({
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

    await expect(page.getByTestId("manual-desk")).toBeVisible();

    const before = await withDb((c) => latestSession(c));

    // There must be NO direct-to-live "place live order" affordance — the only
    // path to live is the guarded switch (proven below). Assert none exists.
    for (const forbidden of [
      "manual-place-live-order",
      "manual-go-live-now",
      "manual-place-order-live",
      "manual-activate-live",
    ]) {
      expect(
        await page.getByTestId(forbidden).count(),
        `no direct-to-live manual affordance "${forbidden}" exists (the only path to live is the guarded switch)`,
      ).toBe(0);
    }

    const liveSwitch = page.getByTestId("manual-mode-live");
    if (!(await liveSwitch.count())) {
      // The desk hides the live switch in this build (paper-only desk) — there is
      // simply no live path in the UI, which is itself safe. Nothing more to drive.
      test.skip(
        true,
        "no switch-to-live control on the desk (live arming hidden in this build).",
      );
      return;
    }

    // Clicking the switch must NOT arm live — it can only open the guarded dialog.
    await liveSwitch.first().click();
    const dialog = page.getByTestId("manual-live-confirm");
    await expect(dialog).toBeVisible();

    const phrase = page.getByTestId("manual-live-confirm-phrase");
    await expect(phrase).toBeVisible();
    const submit = page.getByTestId("manual-live-confirm-submit");

    // GUARD: submit disabled before the phrase is entered.
    const disabledBefore =
      (await submit.isDisabled().catch(() => false)) ||
      (await submit.getAttribute("aria-disabled")) === "true";
    expect(
      disabledBefore,
      "switch-to-live submit is disabled until the exact phrase is typed",
    ).toBeTruthy();

    // A WRONG/near-miss phrase keeps submit disabled (the exact phrase is required).
    await phrase.fill("i confirm this real money manual order");
    const stillDisabled =
      (await submit.isDisabled().catch(() => false)) ||
      (await submit.getAttribute("aria-disabled")) === "true";
    expect(
      stillDisabled,
      "a wrong/near-miss phrase never arms the live switch",
    ).toBeTruthy();

    // CANCEL — we NEVER complete a live switch (no real account in the gate).
    const cancel = page.getByTestId("manual-live-confirm-cancel");
    if (await cancel.count()) {
      await cancel.click();
    } else {
      await page.keyboard.press("Escape");
    }
    await expect(dialog).toBeHidden();

    // Durable truth: NO switch occurred — the session mode is unchanged and NOT
    // live.
    const after = await withDb((c) => latestSession(c));
    if (before && after) {
      expect(
        after.mode,
        "canceling the confirmation left the session mode unchanged",
      ).toBe(before.mode);
    }
    expect(after?.mode, "the desk never auto-armed live").not.toBe("live");
  });
});
