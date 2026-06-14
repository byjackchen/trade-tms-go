/**
 * (5) LIVE SAFETY — live (real money) cannot be activated without the full gate.
 *
 * Switching to LIVE mode is the single most dangerous action in the system: it
 * arms the real-money account. P6 decision 8 makes live activation require ALL
 * of: (a) a typed confirmation phrase, (b) a real acc_id explicitly configured,
 * (c) UnlockTrade success, (d) a distinct trader-id namespace (TMS-LIVE-REAL-001)
 * — and there must be NO code path that places a real order without all four.
 * signal/paper can NEVER reach the live account.
 *
 * This spec asserts the GUARD EXISTS without ever activating live (it must not
 * — there is no real account in this gate). Checks:
 *   (a) API: a set_mode->live WITHOUT a confirm_token is rejected 412
 *       confirmation_required (the boundary guard); the session mode is unchanged
 *       (never silently flipped to live).
 *   (b) UI: the switch-to-live control opens a confirmation-phrase dialog whose
 *       submit is DISABLED until the exact phrase is typed; we verify the guard
 *       and CANCEL — we never complete the switch.
 *   (c) UI never lets signal/paper place to a live account: in signal/paper mode
 *       the cockpit exposes no affordance that routes an order to the live
 *       account; the only path to live is the guarded mode switch above. We
 *       assert there is no enabled "go live" / "place live order" control that
 *       bypasses the confirmation dialog.
 *
 * This is the TOP acceptance criterion (SAFE). It runs whenever the live reader
 * is present; the API guard is always safe (a 412 means live did NOT activate).
 *
 * Testid contract:
 *   control-mode-live            — the switch-to-live control (existing)
 *   live-mode-confirm            — the confirmation dialog (shared, existing)
 *   live-mode-confirm-phrase     — the typed-phrase input (existing)
 *   live-mode-confirm-submit     — the arm/submit button (existing)
 *   live-mode-confirm-cancel     — cancel without switching (existing)
 */

import { test, expect } from "../fixtures/test";
import { postAuthed } from "../lib/api";
import { withDb, latestSession } from "../lib/db";
import { liveUiReady, liveReaderAvailable } from "../lib/live";

test.describe("LIVE safety — real money is never activated without the full gate", () => {
  test("the API rejects a live-mode switch without a confirm token (412)", async () => {
    if (!(await liveReaderAvailable())) {
      test.skip(true, "API started without a live reader (live endpoints 503).");
      return;
    }

    const before = await withDb((c) => latestSession(c));

    // set_mode -> live WITHOUT confirm_token must be rejected at the boundary.
    // A 412 means live did NOT activate — the safety boundary held.
    const res = await postAuthed("live/commands", {
      name: "set_mode",
      mode: "live",
    });
    expect(
      res.status,
      "set_mode->live without confirm_token is a confirmation-required 412",
    ).toBe(412);
    const body = res.body as { error?: { code?: string } } | undefined;
    expect(body?.error?.code).toBe("confirmation_required");

    // The session mode is UNCHANGED — never silently flipped to live. In
    // particular a signal/paper session was not turned live by the rejected call.
    const after = await withDb((c) => latestSession(c));
    if (before && after) {
      expect(
        after.mode,
        "the rejected set_mode left the session mode unchanged",
      ).toBe(before.mode);
      expect(after.mode, "the session is not live after a rejected switch").not.toBe(
        "live",
      );
    }
  });

  test("the cockpit switch-to-live opens a confirmation-phrase dialog and never auto-activates", async ({
    page,
  }) => {
    if (!(await liveUiReady(page))) {
      test.skip(true, "Live cockpit not yet implemented (coming-soon).");
      return;
    }
    if (!(await liveReaderAvailable())) {
      test.skip(true, "API started without a live reader (live endpoints 503).");
      return;
    }

    await expect(page.getByTestId("live-page")).toBeVisible();

    const before = await withDb((c) => latestSession(c));

    const liveSwitch = page.getByTestId("control-mode-live");
    if (!(await liveSwitch.count())) {
      test.skip(
        true,
        "switch-to-live control not surfaced (live activation hidden in this build).",
      );
      return;
    }

    // The live switch must NOT activate on click — it can only open the guarded
    // confirmation dialog. (If the current mode is already live the control is
    // disabled; in the gate the session is signal/paper, so it is actionable.)
    await liveSwitch.first().click();

    const dialog = page.getByTestId("live-mode-confirm");
    await expect(dialog).toBeVisible();

    const phraseInput = page.getByTestId("live-mode-confirm-phrase");
    await expect(phraseInput).toBeVisible();

    const submit = page.getByTestId("live-mode-confirm-submit");
    // GUARD: submit is not actionable before the phrase is entered.
    const disabledBefore =
      (await submit.isDisabled().catch(() => false)) ||
      (await submit.getAttribute("aria-disabled")) === "true";
    expect(
      disabledBefore,
      "switch-to-live submit is disabled until the confirmation phrase is typed",
    ).toBeTruthy();

    // Typing the WRONG phrase must keep submit disabled (the exact phrase is
    // required — a near-miss never arms live).
    await phraseInput.fill("set live");
    const stillDisabled =
      (await submit.isDisabled().catch(() => false)) ||
      (await submit.getAttribute("aria-disabled")) === "true";
    expect(
      stillDisabled,
      "a wrong/near-miss phrase does not arm the live switch",
    ).toBeTruthy();

    // CANCEL — we NEVER complete a live switch (no real account in this gate).
    const cancel = page.getByTestId("live-mode-confirm-cancel");
    if (await cancel.count()) {
      await cancel.click();
    } else {
      await page.keyboard.press("Escape");
    }
    await expect(dialog).toBeHidden();

    // Durable truth: NO switch occurred — the session mode is unchanged and is
    // NOT live.
    const after = await withDb((c) => latestSession(c));
    if (before && after) {
      expect(
        after.mode,
        "canceling the confirmation left the session mode unchanged",
      ).toBe(before.mode);
    }
    const sessionNow = await withDb((c) => latestSession(c));
    expect(
      sessionNow?.mode,
      "the cockpit never auto-activated live",
    ).not.toBe("live");
  });

  test("signal/paper modes expose no control that places to a live account", async ({
    page,
  }) => {
    if (!(await liveUiReady(page))) {
      test.skip(true, "Live cockpit not yet implemented (coming-soon).");
      return;
    }
    if (!(await liveReaderAvailable())) {
      test.skip(true, "API started without a live reader (live endpoints 503).");
      return;
    }

    await expect(page.getByTestId("live-page")).toBeVisible();

    const session = await withDb((c) => latestSession(c));
    // This invariant matters most when NOT already in live mode (signal/paper):
    // there must be no affordance that reaches the live account except the guarded
    // mode switch. If the stack is already live (never in the gate), skip.
    if (session?.mode === "live") {
      test.skip(true, "session already in live mode — not the signal/paper case.");
      return;
    }

    // There is exactly ONE path to live: the guarded `control-mode-live` switch,
    // which routes through `live-mode-confirm`. Any control that claims to place
    // a live order directly (a `live-place-order-live` / `go-live-now` style
    // affordance) would be a safety hole — assert none exists.
    for (const forbidden of [
      "live-place-order-live",
      "go-live-now",
      "activate-live",
      "place-live-order",
    ]) {
      expect(
        await page.getByTestId(forbidden).count(),
        `no direct-to-live affordance "${forbidden}" exists (the only path to live is the guarded mode switch)`,
      ).toBe(0);
    }

    // The flatten/order controls (when present) operate on the CURRENT session's
    // account, which is signal (no account) or paper — never the live account
    // without the mode switch. We assert the live switch, if present, still
    // requires the confirmation dialog (proven by the test above) rather than
    // being a one-click activation.
    const liveSwitch = page.getByTestId("control-mode-live");
    if (await liveSwitch.count()) {
      // The control is a mode-switch button, not a live-order placer: clicking it
      // opens a dialog (guarded) — it never submits to the live account directly.
      // (Behaviour proven in the test above; here we only assert it is not some
      // auto-submitting element by checking it is a button-like control.)
      const tag = await liveSwitch
        .first()
        .evaluate((el) => el.tagName.toLowerCase());
      expect(
        ["button", "a"].includes(tag),
        "the switch-to-live control is a guarded button, not an auto-activating element",
      ).toBeTruthy();
    }
  });
});
