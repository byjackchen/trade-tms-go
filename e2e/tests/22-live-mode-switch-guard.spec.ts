/**
 * (5) Live cockpit — mode-switch to paper requires a confirmation phrase.
 *
 * Switching the node out of signal mode (to paper/live) is the most dangerous
 * control: paper/live trade real or simulated money (paper/live are deferred to
 * P6, but the GUARD must already exist). The command contract requires a
 * `confirm_token` for `set_mode` to paper/live, and the API returns
 * 412 confirmation_required without it (docs/api.md "Live (P5)").
 *
 * This spec asserts the GUARD EXISTS — it deliberately does NOT complete the
 * switch (no real mode change). Two independent checks:
 *   (a) UI: clicking the mode-switch-to-paper control opens a confirmation-phrase
 *       dialog whose submit is disabled until the exact phrase is typed; we
 *       verify the dialog + the disabled-submit guard, then CANCEL.
 *   (b) API: a set_mode→paper command WITHOUT a confirm_token is rejected with
 *       412 confirmation_required (the boundary guard the UI dialog protects).
 *
 * Self-skips: coming-soon placeholder or no live reader (503). The API guard
 * check (b) is safe to run whenever the live reader is present (it never
 * mutates — a 412 means the switch did NOT happen).
 */

import { test, expect } from "../fixtures/test";
import { postAuthed } from "../lib/api";
import {
  withDb,
  latestSession,
} from "../lib/db";
import { liveUiReady, liveReaderAvailable } from "../lib/live";

test.describe("live cockpit — mode-switch confirmation guard", () => {
  test("the API rejects a paper-mode switch without a confirm token (412)", async () => {
    if (!(await liveReaderAvailable())) {
      test.skip(true, "API started without a live reader (live endpoints 503).");
      return;
    }

    const before = await withDb((c) => latestSession(c));

    // set_mode -> paper WITHOUT confirm_token must be rejected at the boundary.
    const res = await postAuthed("live/commands", {
      name: "set_mode",
      mode: "paper",
    });
    expect(
      res.status,
      "set_mode->paper without confirm_token is a confirmation-required 412",
    ).toBe(412);
    const body = res.body as { error?: { code?: string } } | undefined;
    expect(body?.error?.code).toBe("confirmation_required");

    // The guard means the switch did NOT happen: the latest session's mode is
    // unchanged (still whatever it was — never silently flipped to paper).
    const after = await withDb((c) => latestSession(c));
    if (before && after) {
      expect(
        after.mode,
        "rejected set_mode left the session mode unchanged",
      ).toBe(before.mode);
      // Specifically, the rejected attempt did not turn a signal session into
      // paper.
      if (before.mode === "signal") {
        expect(after.mode).toBe("signal");
      }
    }
  });

  test("the cockpit mode-switch opens a confirmation-phrase dialog (no actual switch)", async ({
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

    // The mode-switch control. The cockpit exposes a switch-to-paper affordance
    // guarded behind a confirmation-phrase dialog; if the control is not present
    // (paper/live deferred to P6 and the UI may hide it), the test skips — but
    // the API guard (other test) still proves the boundary protection exists.
    const modeSwitch = page.getByTestId("live-mode-switch-paper");
    if (!(await modeSwitch.count())) {
      test.skip(
        true,
        "mode-switch-to-paper control not surfaced (paper deferred to P6).",
      );
      return;
    }

    await modeSwitch.first().click();

    // A confirmation-phrase dialog opens. Its submit must be DISABLED until the
    // exact phrase is typed (the destructive-action guard). We assert the guard,
    // then CANCEL — we never complete the switch.
    const dialog = page.getByTestId("live-mode-confirm");
    await expect(dialog).toBeVisible();

    const phraseInput = page.getByTestId("live-mode-confirm-phrase");
    await expect(phraseInput).toBeVisible();

    const submit = page.getByTestId("live-mode-confirm-submit");
    // Guard: submit is not actionable before the phrase is entered. Accept
    // either a disabled attribute or aria-disabled (the UI may use either).
    const disabledBefore =
      (await submit.isDisabled().catch(() => false)) ||
      (await submit.getAttribute("aria-disabled")) === "true";
    expect(
      disabledBefore,
      "mode-switch submit is disabled until the confirmation phrase is typed",
    ).toBeTruthy();

    // Cancel without switching. The cockpit exposes a cancel/close affordance.
    const cancel = page.getByTestId("live-mode-confirm-cancel");
    if (await cancel.count()) {
      await cancel.click();
    } else {
      await page.keyboard.press("Escape");
    }
    await expect(dialog).toBeHidden();

    // Durable truth: NO switch occurred — the session mode is unchanged.
    const after = await withDb((c) => latestSession(c));
    if (before && after) {
      expect(
        after.mode,
        "canceling the confirmation left the session mode unchanged",
      ).toBe(before.mode);
    }
  });
});
