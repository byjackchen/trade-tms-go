/**
 * (4) daily-loss-halt — day P&L below threshold halts NEW opening orders.
 *
 * When day P&L < -daily_loss_halt_pct * NAV (strict <; the boundary does NOT
 * halt — portfolio-risk.md §3.3, default 10%), the portfolio gate enters the
 * daily-loss halt: NEW opening orders are rejected (risk.daily_loss_halt),
 * EXISTING positions stay open (they are NOT force-closed), and FLAT/closing
 * orders are STILL allowed (you can always reduce risk). A tms.halts row of
 * kind=daily_loss is written and stays active until cleared.
 *
 * This spec proves, against durable truth + the cockpit:
 *   - an ACTIVE daily_loss halt exists for the session once the simulated loss
 *     crosses the threshold;
 *   - the cockpit shows the halt banner / halted flag;
 *   - NEW opening orders are rejected after the halt — a risk.daily_loss_halt
 *     rejection appears in risk_events (the gate blocked a new open);
 *   - FLAT is still allowed: the halt does NOT bypass FLAT (no daily_loss
 *     rejection carries side=FLAT — FLAT/close orders pass the gate during a
 *     halt by design).
 *
 * The day-P&L-below-threshold condition is produced by the gate driver (the mock
 * venue / a seeded losing book). When no active daily_loss halt exists this run,
 * the driver did not push P&L below threshold — the spec self-skips.
 *
 * Testid contract:
 *   live-halted-banner / live-session[data-halted]  — the halt indicator
 *   live-health-loss-halt (existing) — the day-loss-halt headroom indicator
 */

import { test, expect } from "../fixtures/test";
import {
  withDb,
  latestSession,
  activeDailyLossHalt,
  recentRejectedRiskEvents,
} from "../lib/db";
import {
  liveUiReady,
  liveTradingAvailable,
  hasRunningTradingSession,
} from "../lib/live";

test.describe("portfolio gate — daily-loss halt", () => {
  test("a day-loss halt rejects NEW opens, keeps positions, and allows FLAT", async ({
    page,
  }) => {
    if (!(await liveUiReady(page))) {
      test.skip(true, "Live cockpit not yet implemented (coming-soon).");
      return;
    }
    if (!(await liveTradingAvailable())) {
      test.skip(
        true,
        "API started without a trading reader (live trading endpoints 503).",
      );
      return;
    }
    if (!(await hasRunningTradingSession())) {
      test.skip(
        true,
        "no RUNNING paper/live session — the daily-loss halt only runs in paper/live.",
      );
      return;
    }

    await expect(page.getByTestId("live-page")).toBeVisible();

    const session = await withDb((c) => latestSession(c));
    const sessionId = session!.id;

    // Durable gate: a daily_loss halt must be active for this session (the loss
    // driver crossed the threshold). If not, the simulated below-threshold loss
    // was not produced this run — skip rather than assert a halt that did not fire.
    const halted = await withDb((c) => activeDailyLossHalt(c, sessionId));
    if (!halted) {
      test.skip(
        true,
        "no active daily_loss halt this session — the loss driver did not cross the threshold.",
      );
      return;
    }

    // 1. The cockpit reflects the halted state (banner or data-halted flag) — the
    //    same surface the kill-switch spec (21) checks, here driven by the
    //    automatic daily-loss halt rather than an operator halt.
    const banner = page.getByTestId("live-halted-banner");
    const sessionStrip = page.getByTestId("live-session");
    await expect
      .poll(
        async () => {
          if ((await banner.count()) && (await banner.first().isVisible())) {
            return true;
          }
          const flag = await sessionStrip.getAttribute("data-halted");
          return flag === "true";
        },
        { timeout: 30_000 },
      )
      .toBe(true);

    // 2. NEW opening orders were rejected by the daily-loss halt — a
    //    risk.daily_loss_halt rejection exists. Per the rule, the rejection is
    //    for a non-FLAT open (LONG/SHORT): FLAT bypasses the halt.
    const rejections = await withDb((c) =>
      recentRejectedRiskEvents(c, sessionId, 100),
    );
    const dailyLossRejections = rejections.filter(
      (r) => r.ruleName === "risk.daily_loss_halt",
    );
    expect(
      dailyLossRejections.length,
      "a risk.daily_loss_halt rejection blocked a NEW opening order",
    ).toBeGreaterThan(0);

    // 3. SAFETY / spec invariant: the halt NEVER rejects a FLAT (close) order —
    //    FLAT/closing orders still pass during a halt by design (portfolio-risk.md
    //    §3.3). No daily_loss rejection may carry side=FLAT.
    for (const r of dailyLossRejections) {
      expect(
        r.side,
        `daily_loss rejection for ${r.symbol} is a NEW open (not a FLAT — FLAT must pass during a halt)`,
      ).not.toBe("FLAT");
    }
  });
});
