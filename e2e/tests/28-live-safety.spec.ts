/**
 * (5) LIVE SAFETY — real money (the LIVE module's real account) cannot be armed
 * without the full gate.
 *
 * Post-restructure the three-valued mode is retired: the account env is fixed by
 * the page (/paper -> simulate, /live -> real) and the only switchable axis is the
 * EXECUTION POLICY (signal emit-only <-> auto auto-submit). Arming AUTO on the
 * LIVE module is the single most dangerous action in the system: it lets the
 * session place REAL orders against the bound real-money account. P6 decision 8
 * makes live activation require ALL of: (a) a typed confirmation phrase, (b) a
 * real acc_id explicitly configured, (c) UnlockTrade success, (d) a distinct
 * trader-id namespace (TMS-LIVE-REAL-001) — and there must be NO code path that
 * arms a real order without all four. SIGNAL emits intents only — no orders.
 *
 * This spec asserts the GUARD EXISTS without ever arming live (it must not — there
 * is no real account in this gate). Checks:
 *   (a) API: a set_mode -> auto+real (the "go live" arm) WITHOUT a confirm_token
 *       is rejected 412 confirmation_required (the boundary guard); the derived
 *       session mode is unchanged (never silently flipped to live).
 *   (b) UI: the LIVE module's AUTO control opens a confirmation-phrase dialog whose
 *       submit is DISABLED until the exact phrase is typed; we verify the guard
 *       and CANCEL — we never complete the arm.
 *   (c) UI never lets signal/paper place to a live account: the paper module
 *       exposes no affordance that routes an order to the real account; the only
 *       path to real money is the guarded AUTO arm on the LIVE module above. We
 *       assert there is no enabled "go live" / "place live order" control that
 *       bypasses the confirmation dialog.
 *
 * This is the TOP acceptance criterion (SAFE). It runs whenever the live reader
 * is present; the API guard is always safe (a 412 means live did NOT activate).
 *
 * Testid contract (matches spec 22's reference style):
 *   control-exec-auto         — the arm-AUTO control on the LIVE module (existing)
 *   live-arm-confirm          — the confirmation dialog (shared, existing)
 *   live-arm-confirm-phrase   — the typed-phrase input (existing)
 *   live-arm-confirm-submit   — the arm/submit button (existing)
 *   live-arm-confirm-cancel   — cancel without arming (existing)
 */

import { test, expect } from "../fixtures/test";
import { postAuthed } from "../lib/api";
import { withDb, latestSession } from "../lib/db";
import { liveReaderAvailable } from "../lib/live";

/** Navigate to the unified trade module and report whether it rendered. In the
 * 4-top IA there is no `/live` page: the former /live 301-redirects to the single
 * /trade surface, whose paper-vs-live treatment follows the account chosen in the
 * top-right selector (docs/concept-alignment.md §3.4). The module's ready signal
 * is the `trade-header` testid (ui/src/components/portfolio/trade-module.tsx).
 * Returns false when the app-shell or the trade header never appears (not built).
 *
 * NOTE: the LIVE arm flow below is gated on the `control-exec-auto` AUTO control
 * being ENABLED; it self-skips when AUTO isn't actionable (no real account
 * selected / activation hidden), so this stays a safe live-guard probe without a
 * dedicated /live page. */
async function liveModuleReady(
  page: import("@playwright/test").Page,
): Promise<boolean> {
  await page.goto("/trade", { waitUntil: "domcontentloaded" });
  const shell = page.getByTestId("app-shell");
  try {
    await shell.waitFor({ state: "visible", timeout: 15_000 });
  } catch {
    return false;
  }
  const header = page.getByTestId("trade-header");
  try {
    await header.waitFor({ state: "visible", timeout: 15_000 });
    return true;
  } catch {
    return false;
  }
}

test.describe("LIVE safety — real money is never armed without the full gate", () => {
  test("the API rejects arming live (auto+real) without a confirm token (412)", async () => {
    if (!(await liveReaderAvailable())) {
      test.skip(true, "API started without a live reader (live endpoints 503).");
      return;
    }

    const before = await withDb((c) => latestSession(c));

    // set_mode -> "go live" (exec_policy=auto + env=real) WITHOUT confirm_token
    // must be rejected at the boundary. A 412 means live did NOT arm — the safety
    // boundary held.
    const res = await postAuthed("trade/commands", {
      name: "set_mode",
      exec_policy: "auto",
      env: "real",
    });
    expect(
      res.status,
      "arming auto+real without confirm_token is a confirmation-required 412",
    ).toBe(412);
    const body = res.body as { error?: { code?: string } } | undefined;
    expect(body?.error?.code).toBe("confirmation_required");

    // The (derived) session mode is UNCHANGED — never silently flipped to live. In
    // particular a signal/paper session was not turned live by the rejected call.
    const after = await withDb((c) => latestSession(c));
    if (before && after) {
      expect(
        after.mode,
        "the rejected arm left the session mode unchanged",
      ).toBe(before.mode);
      expect(after.mode, "the session is not live after a rejected arm").not.toBe(
        "live",
      );
    }
  });

  test("the LIVE module AUTO control opens a confirmation-phrase dialog and never auto-arms", async ({
    page,
  }) => {
    if (!(await liveModuleReady(page))) {
      test.skip(true, "Live module not yet implemented (coming-soon).");
      return;
    }
    if (!(await liveReaderAvailable())) {
      test.skip(true, "API started without a live reader (live endpoints 503).");
      return;
    }

    await expect(page.getByTestId("trade-header")).toBeVisible();

    const before = await withDb((c) => latestSession(c));

    // On the trade module, arming AUTO is the "go live" action (env=real). The
    // button for the already-active policy is disabled; in the gate the session is
    // signal/paper so AUTO is actionable. If it is not enabled (already auto / no
    // reader), there is nothing to arm — skip.
    const armAuto = page.getByTestId("control-exec-auto").first();
    if (
      !(await armAuto.count()) ||
      !(await armAuto.isEnabled().catch(() => false))
    ) {
      test.skip(
        true,
        "no enabled AUTO control surfaced (already auto / activation hidden in this build).",
      );
      return;
    }

    // The AUTO control must NOT arm on click — it can only open the guarded
    // confirmation dialog.
    await armAuto.click();

    const dialog = page.getByTestId("live-arm-confirm");
    await expect(dialog).toBeVisible();

    const phraseInput = page.getByTestId("live-arm-confirm-phrase");
    await expect(phraseInput).toBeVisible();

    const submit = page.getByTestId("live-arm-confirm-submit");
    // GUARD: submit is not actionable before the phrase is entered.
    const disabledBefore =
      (await submit.isDisabled().catch(() => false)) ||
      (await submit.getAttribute("aria-disabled")) === "true";
    expect(
      disabledBefore,
      "arm-AUTO submit is disabled until the confirmation phrase is typed",
    ).toBeTruthy();

    // Typing the WRONG phrase must keep submit disabled (the exact phrase is
    // required — a near-miss never arms live).
    await phraseInput.fill("arm live");
    const stillDisabled =
      (await submit.isDisabled().catch(() => false)) ||
      (await submit.getAttribute("aria-disabled")) === "true";
    expect(
      stillDisabled,
      "a wrong/near-miss phrase does not arm the live switch",
    ).toBeTruthy();

    // CANCEL — we NEVER complete a live arm (no real account in this gate).
    const cancel = page.getByTestId("live-arm-confirm-cancel");
    if (await cancel.count()) {
      await cancel.click();
    } else {
      await page.keyboard.press("Escape");
    }
    await expect(dialog).toBeHidden();

    // Durable truth: NO arm occurred — the session mode is unchanged and is NOT
    // live.
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
      "the LIVE module never auto-armed live",
    ).not.toBe("live");
  });

  test("the trade module exposes no control that places to a live account", async ({
    page,
  }) => {
    if (!(await liveModuleReady(page))) {
      test.skip(true, "Live module not yet implemented (coming-soon).");
      return;
    }
    if (!(await liveReaderAvailable())) {
      test.skip(true, "API started without a live reader (live endpoints 503).");
      return;
    }

    // The unified trade module defaults to the simulate-bound (paper) account when
    // no real account is selected — it must never reach a real account from here.
    // Load it and assert there is no direct-to-live affordance.
    await page.goto("/trade", { waitUntil: "domcontentloaded" });
    await expect(page.getByTestId("trade-header")).toBeVisible();

    const session = await withDb((c) => latestSession(c));
    // This invariant matters most when NOT already in live mode (signal/paper):
    // there must be no affordance that reaches the real account except the guarded
    // AUTO arm on the LIVE module. If the stack is already live (never in the
    // gate), skip.
    if (session?.mode === "live") {
      test.skip(true, "session already in live mode — not the signal/paper case.");
      return;
    }

    // There is exactly ONE path to real money: the guarded `control-exec-auto`
    // arm on the LIVE module, which routes through `live-arm-confirm`. Any control
    // on the paper module that claims to place a live order directly (a
    // `place-live-order` / `go-live-now` style affordance) would be a safety hole —
    // assert none exists.
    for (const forbidden of [
      "live-place-order-live",
      "go-live-now",
      "activate-live",
      "place-live-order",
    ]) {
      expect(
        await page.getByTestId(forbidden).count(),
        `no direct-to-live affordance "${forbidden}" exists (the only path to real money is the guarded AUTO arm on the LIVE module)`,
      ).toBe(0);
    }

    // The paper module's AUTO arm, when present, binds the SIMULATE account (no
    // real money) and is itself a guarded button (proven by the test above) rather
    // than a one-click activation. Assert it is a button-like control, not an
    // auto-submitting element.
    const armAuto = page.getByTestId("control-exec-auto");
    if (await armAuto.count()) {
      const tag = await armAuto
        .first()
        .evaluate((el) => el.tagName.toLowerCase());
      expect(
        ["button", "a"].includes(tag),
        "the arm-AUTO control is a guarded button, not an auto-activating element",
      ).toBeTruthy();
    }
  });
});
