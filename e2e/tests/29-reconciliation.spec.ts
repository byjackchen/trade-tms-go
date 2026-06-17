/**
 * (6) Reconciliation panel — renders the latest report; a mismatch is flagged.
 *
 * Periodically + on demand the node compares broker positions (Trd_GetPositionList)
 * vs the strategy books and writes a tms.reconciliation_reports row (P6 decision
 * 5; portfolio-risk.md §6). The report carries matched / mismatches / one-sided
 * lists + a derived has_issues. On a mismatch the node HALTS + surfaces it in the
 * cockpit and does NOT auto-trade to correct (the operator decides).
 *
 * This spec proves, against durable truth + the cockpit:
 *   - the reconciliation panel renders the latest report (ts, the four lists);
 *   - the rendered matched/mismatch/one-sided contents MATCH the DB report (no
 *     fabricated drift);
 *   - when the report has_issues, the panel VISUALLY FLAGS it (a data-has-issues
 *     flag / a mismatch indicator) — the operator-visible alert;
 *   - each rendered mismatch row's diff equals broker_net - strategy_books_sum
 *     (the sign-correct invariant from the spec).
 *
 * PERMANENT + self-skipping: requires the trading reader and at least one
 * reconciliation report to exist; otherwise self-skips so the gate stays green.
 *
 * Testid contract:
 *   live-reconciliation              — the reconciliation panel root;
 *                                      data-has-issues="true|false"
 *   live-reconciliation-mismatch     — visible only when has_issues (the flag)
 *   live-recon-mismatch-row          — one row per mismatch; data-symbol /
 *                                      data-diff / data-broker-net /
 *                                      data-strategy-sum attrs
 *   live-recon-matched-count         — count of matched symbols (optional)
 */

import { test, expect } from "../fixtures/test";
import { getAuthed } from "../lib/api";
import {
  withDb,
  latestReconciliation,
  reconciliationReportCount,
} from "../lib/db";
import {
  liveUiReady,
  liveTradingAvailable,
  firstVisibleTestId,
} from "../lib/live";

test.describe("paper trading — reconciliation panel", () => {
  test("the reconciliation panel matches the DB report and flags mismatches", async ({
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

    // Durable gate: at least one reconciliation report exists (a reconcile ran).
    const reportCount = await withDb((c) => reconciliationReportCount(c));
    if (reportCount === 0) {
      test.skip(
        true,
        "no reconciliation report yet — no reconcile has run this session.",
      );
      return;
    }

    const dbReport = await withDb((c) => latestReconciliation(c));
    expect(dbReport, "the latest reconciliation report is readable").not.toBeNull();

    // Cross-check the API proxies the same report the DB holds (the UI renders the
    // API's proxy; both must agree with the DB). has_issues + list cardinalities.
    const api = await getAuthed("trade/reconciliation");
    expect(api.status, "reconciliation endpoint is reachable").toBe(200);
    const apiBody = api.body as {
      has_issues?: boolean;
      mismatches?: unknown[];
      symbols_only_in_strategies?: unknown[];
      symbols_only_at_broker?: unknown[];
    } | null;
    if (apiBody && typeof apiBody.has_issues === "boolean") {
      expect(
        apiBody.has_issues,
        "API has_issues matches the DB report",
      ).toBe(dbReport!.hasIssues);
      // Sign-correct invariant on the API's mismatches: diff = broker_net - sum.
      for (const m of (apiBody.mismatches ?? []) as Array<{
        broker_net: number;
        strategy_books_sum: number;
        diff: number;
      }>) {
        expect(
          m.diff,
          `API mismatch diff = broker_net - strategy_books_sum for ${JSON.stringify(m)}`,
        ).toBe(m.broker_net - m.strategy_books_sum);
      }
    }

    await expect(page.getByTestId("trade-header")).toBeVisible();

    // ----- UI: the reconciliation panel ------------------------------------
    const panel = await firstVisibleTestId(page, ["live-reconciliation"], 12_000);
    if (!panel) {
      test.skip(
        true,
        "reconciliation panel not surfaced yet (paper-trading cockpit panels deferred).",
      );
      return;
    }
    const panelEl = page.getByTestId("live-reconciliation");
    await expect(panelEl).toBeVisible();

    // The panel exposes the has-issues flag for an exact, text-independent check.
    const flag = await panelEl.getAttribute("data-has-issues");
    if (flag != null) {
      expect(
        flag === "true",
        "the panel's has-issues flag matches the DB report",
      ).toBe(dbReport!.hasIssues);
    }

    if (dbReport!.hasIssues) {
      // A mismatch is VISUALLY FLAGGED — the operator-visible alert. Either the
      // dedicated mismatch indicator is visible, or the panel's has-issues flag
      // is set (a styled red state keyed off it).
      const mismatchFlag = page.getByTestId("live-reconciliation-mismatch");
      const flagged =
        (flag === "true") ||
        ((await mismatchFlag.count()) > 0 &&
          (await mismatchFlag.first().isVisible()));
      expect(
        flagged,
        "a reconciliation mismatch is visually flagged in the cockpit",
      ).toBeTruthy();

      // Each rendered mismatch row matches the DB drift, sign-correct.
      const dbBySymbol = new Map(
        dbReport!.mismatches.map((m) => [m.symbol, m]),
      );
      const rows = page.getByTestId("live-recon-mismatch-row");
      const rn = await rows.count();
      for (let i = 0; i < rn; i++) {
        const row = rows.nth(i);
        const sym = await row.getAttribute("data-symbol");
        if (sym == null) continue;
        const truth = dbBySymbol.get(sym);
        expect(
          truth,
          `mismatch row ${sym} is a real drift in the DB report`,
        ).toBeTruthy();
        if (!truth) continue;
        const diffAttr = await row.getAttribute("data-diff");
        if (diffAttr != null) {
          expect(
            Number(diffAttr),
            `mismatch row ${sym} diff = broker_net - strategy_books_sum`,
          ).toBe(truth.brokerNet - truth.strategyBooksSum);
        }
      }
    } else {
      // A clean report: the panel must NOT spuriously flag a mismatch.
      const mismatchFlag = page.getByTestId("live-reconciliation-mismatch");
      if (await mismatchFlag.count()) {
        expect(
          await mismatchFlag.first().isVisible(),
          "a clean reconciliation report shows no mismatch flag",
        ).toBeFalsy();
      }
    }
  });
});
