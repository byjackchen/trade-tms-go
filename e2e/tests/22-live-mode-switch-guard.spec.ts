/**
 * (5) Trade module — arming AUTO requires a confirmation phrase.
 *
 * In the two-module design the account env is fixed by the page (paper ->
 * simulate, live -> real); the only switchable axis is the EXECUTION POLICY:
 * SIGNAL (emit-only) <-> AUTO (auto-submit). Arming AUTO is the dangerous control
 * (auto-submits orders — real money on the live module). The command contract
 * requires a `confirm_token` for `set_mode` with exec_policy=auto, and the API
 * returns 412 confirmation_required without it (docs §1.3, C6).
 *
 * This spec asserts the GUARD EXISTS — it deliberately does NOT complete the arm
 * (no real exec-policy change). Two independent checks:
 *   (a) UI: clicking the AUTO control opens a confirmation-phrase dialog whose
 *       submit is disabled until the exact phrase is typed; we verify the dialog
 *       + the disabled-submit guard, then CANCEL.
 *   (b) API: a set_mode (exec_policy=auto) command WITHOUT a confirm_token is
 *       rejected with 412 confirmation_required (the boundary guard the dialog
 *       protects). A 412 means the arm did NOT happen — never mutates.
 *
 * Self-skips: coming-soon placeholder or no live reader (503).
 */

import { test, expect } from "../fixtures/test";
import { postAuthed } from "../lib/api";
import { liveUiReady, liveReaderAvailable } from "../lib/live";

test.describe("trade module — arm-AUTO confirmation guard", () => {
  test("the API rejects arming AUTO without a confirm token (412)", async () => {
    if (!(await liveReaderAvailable())) {
      test.skip(true, "API started without a live reader (live endpoints 503).");
      return;
    }

    // set_mode -> exec_policy=auto WITHOUT confirm_token must be rejected at the
    // boundary. Use env=paper so the check never touches real money.
    const res = await postAuthed("trade/commands", {
      name: "set_mode",
      exec_policy: "auto",
      env: "paper",
    });
    expect(
      res.status,
      "arming AUTO without confirm_token is a confirmation-required 412",
    ).toBe(412);
    const body = res.body as { error?: { code?: string } } | undefined;
    expect(body?.error?.code).toBe("confirmation_required");
  });

  test("the trade module AUTO control opens a confirmation-phrase dialog (no actual arm)", async ({
    page,
  }) => {
    if (!(await liveUiReady(page))) {
      test.skip(true, "Trade module not ready (coming-soon / not implemented).");
      return;
    }
    if (!(await liveReaderAvailable())) {
      test.skip(true, "API started without a live reader (live endpoints 503).");
      return;
    }

    // The AUTO control opens a confirmation-phrase dialog (SIGNAL switches
    // directly — no money at risk). The button for the policy already active is
    // (correctly) disabled. If the session is already AUTO, there is nothing to
    // arm — skip.
    const armAuto = page.getByTestId("control-exec-auto").first();
    if (
      !(await armAuto.count()) ||
      !(await armAuto.isEnabled().catch(() => false))
    ) {
      test.skip(true, "no enabled AUTO control surfaced (already auto / no reader).");
      return;
    }

    await armAuto.click();

    // A confirmation-phrase dialog opens. Its submit must be DISABLED until the
    // exact phrase is typed (the destructive-action guard). Assert the guard,
    // then CANCEL — we never complete the arm.
    const dialog = page.getByTestId("live-arm-confirm");
    await expect(dialog).toBeVisible();

    const submit = page.getByTestId("live-arm-confirm-submit");
    const disabledBefore =
      (await submit.isDisabled().catch(() => false)) ||
      (await submit.getAttribute("aria-disabled")) === "true";
    expect(
      disabledBefore,
      "arm-AUTO submit is disabled until the confirmation phrase is typed",
    ).toBeTruthy();

    // Cancel without arming.
    const cancel = page.getByTestId("live-arm-confirm-cancel");
    if (await cancel.count()) {
      await cancel.click();
    } else {
      await page.keyboard.press("Escape");
    }
    await expect(dialog).toBeHidden();
  });
});
