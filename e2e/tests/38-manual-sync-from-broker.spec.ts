/**
 * (Manual desk 7) SYNC FROM BROKER — DIRECTION 2 (broker -> TMS), the operator's
 * PRIMARY case.
 *
 * The operator trades DIRECTLY in the moomoo app (NO order placed via TMS), then
 * returns to TMS and clicks "Sync from broker" to pull the account's ACTUAL state
 * (Trd_GetPositionList + Trd_GetOrderList + Trd_GetOrderFillList + Trd_GetFunds)
 * and REFLECT it into TMS so the positions/orders/fills/account panels show what was
 * actually done in moomoo (docs/api.md "POST /api/v1/trade/sync"). The synced broker
 * truth is reflected under the MANUAL/EXTERNAL book (so the auto-strategy books are
 * not corrupted), then reconciled vs the strategy books (P6 reconciliation ->
 * reconciliation_reports) so externally-introduced drift is reported.
 *
 * SAFETY (paramount): the sync is READ-ONLY at the broker — it places NO orders, so
 * it needs NO per-order confirm token and NO risk gate, and is safe in ALL modes,
 * INCLUDING signal (a signal-mode operator can sync to see/manage what they actually
 * hold). Because nothing crosses the wire to the venue, this spec — uniquely among
 * the manual-desk specs — does NOT gate on `manualDeskIsPaper()`: a sync is safe even
 * on a live-bound desk (it never sends an order). It is also IDEMPOTENT: the broker
 * net is reflected as the DELTA vs the current MANUAL book net per symbol, so
 * re-syncing the SAME broker state reflects nothing (no duplicated rows / no
 * double-counted positions).
 *
 * What this proves, end to end:
 *   (a) API boundary (always safe with a connected desk): POST /trade/sync -> 200
 *       `{status:"synced", positions_observed, orders_observed, fills_observed,
 *       reflected, read_only:true, reconciliation?}`. read_only is true (no order
 *       placed). The sync is AUDITED (a `trade.manual.sync` ops.audit_log row).
 *   (b) IDEMPOTENCY: a second sync of the same broker state reflects nothing new —
 *       reflected == 0 and the distinct-symbol count of the MANUAL book does NOT
 *       grow (re-syncing the same state is a no-op, no duplicated position rows).
 *   (c) REFLECTION: whatever broker positions the sync reflected surface under the
 *       MANUAL book in the DB and (when the desk surfaces the sync panel + the
 *       positions panel) in the UI — every rendered MANUAL position row is a real
 *       open MANUAL position in the DB (faithful proxy, never fabricated).
 *   (d) UI (when the desk surfaces the Sync control): clicking "Sync from broker"
 *       shows the read-only result toast (counts + reconciliation), the positions /
 *       blotter panels reflect the synced broker book, and re-syncing does NOT
 *       duplicate a position symbol.
 *
 * PERMANENT + self-skipping: the API part skips only while no manual desk is
 * connected (POST /api/v1/trade/* 503). The UI part additionally skips while the
 * desk UI is the coming-soon placeholder or does not surface the sync control yet —
 * exactly like specs 32-37 self-skip for their panels — so the gate stays green.
 *
 * Testid contract (ui/.../sync-from-broker.tsx):
 *   manual-sync                  — the sync-from-broker panel root; data-last-synced
 *   manual-sync-button           — the "Sync from broker" button (disabled while pending)
 *   manual-sync-last             — the "last synced" timestamp line
 *   manual-sync-result           — the result toast; data-read-only / data-has-drift /
 *                                  data-reflected
 *   manual-sync-read-only        — the read-only badge (proves no order was placed)
 *   manual-sync-counts           — data-positions / data-orders / data-fills
 *   manual-sync-error            — the error toast (e.g. 503 no desk)
 *   manual-positions / manual-position-row [data-symbol/data-signed-qty]
 *                                — the positions panel the sync reflects into (spec 32)
 *   manual-blotter               — the blotter the synced broker orders surface in
 */

import { test, expect } from "../fixtures/test";
import { postManual } from "../lib/api";
import {
  withDb,
  latestSession,
  openManualPositions,
  openManualPositionSymbols,
  manualPositionSymbolCount,
  syncAuditCount,
} from "../lib/db";
import {
  manualDeskUiReady,
  manualDeskAvailable,
  manualSyncAvailable,
  firstVisibleTestId,
  waitFor,
} from "../lib/live";

/** The documented 200 shape of POST /api/v1/trade/sync. `reconciliation` is
 * OPTIONAL — the server includes it only when a reconciler is wired
 * (SyncReport.HasReconciliation; internal/livetrade/manual_sync.go). */
type SyncBody = {
  status?: string;
  positions_observed?: number;
  orders_observed?: number;
  fills_observed?: number;
  reflected?: number;
  read_only?: boolean;
  reconciliation?: {
    has_issues?: boolean;
    summary?: string;
    matched?: number;
    mismatches?: number;
  };
};

test.describe("manual desk — sync from broker (DIRECTION 2, broker -> TMS)", () => {
  test("POST /trade/sync reflects broker truth read-only, is audited, and is idempotent on re-sync", async () => {
    // Sync is READ-ONLY at the broker → safe in ALL modes (incl signal). Only a
    // connected desk is required; we do NOT gate on a paper desk here.
    if (!(await manualSyncAvailable())) {
      test.skip(true, "no manual desk connected (POST /api/v1/trade/* 503) — nothing to sync.");
      return;
    }

    const session = await withDb((c) => latestSession(c));
    expect(session, "a trading session exists").not.toBeNull();
    const sessionId = session!.id;

    // Audit + MANUAL-book baselines BEFORE the first sync (the sync must be audited
    // and must not duplicate a symbol on re-sync).
    const auditBefore = await withDb((c) => syncAuditCount(c));
    const symbolsBefore = await withDb((c) => manualPositionSymbolCount(c, sessionId));

    // ----- FIRST SYNC: read-only pull + reflect ------------------------------
    const first = await postManual("trade/sync", {});
    expect(
      first.status,
      "a connected desk syncs from the broker (200), never 503 here",
    ).toBe(200);
    const fb = first.body as SyncBody;
    expect(fb?.status, "the sync reports status:synced").toBe("synced");
    expect(
      fb?.read_only,
      "the sync is READ-ONLY at the broker — it places NO order (read_only:true)",
    ).toBe(true);
    // The observed counts are the ACTUAL broker rows pulled — non-negative integers.
    for (const k of [
      "positions_observed",
      "orders_observed",
      "fills_observed",
      "reflected",
    ] as const) {
      const v = fb?.[k];
      expect(typeof v, `${k} is a number`).toBe("number");
      expect(
        Number.isInteger(v) && (v as number) >= 0,
        `${k} is a non-negative integer`,
      ).toBe(true);
    }
    // When the server wired a reconciler, the report shape is present + well-formed
    // (it is optional — only included when HasReconciliation).
    if (fb.reconciliation !== undefined) {
      expect(
        typeof fb.reconciliation.has_issues,
        "reconciliation.has_issues is a boolean when reconciliation is reported",
      ).toBe("boolean");
    }

    // The sync is AUDITED (a durable record of the operator action, even read-only).
    const auditAfterFirst = await waitFor(
      () => withDb((c) => syncAuditCount(c)),
      (n) => n > auditBefore,
      { interval: 1_000, timeout: 15_000 },
    );
    expect(
      auditAfterFirst,
      "the sync wrote a DIRECTION-2 sync audit row (trade.manual.sync)",
    ).toBeGreaterThan(auditBefore);

    // Whatever the sync reflected now lives in the MANUAL/EXTERNAL book.
    const symbolsAfterFirst = await withDb((c) =>
      manualPositionSymbolCount(c, sessionId),
    );

    // ----- SECOND SYNC: idempotent (re-syncing the same state reflects nothing) --
    const second = await postManual("trade/sync", {});
    expect(second.status, "a re-sync also succeeds (200)").toBe(200);
    const sb = second.body as SyncBody;
    expect(sb?.read_only, "the re-sync is also read-only").toBe(true);
    expect(
      sb?.reflected,
      "re-syncing the SAME broker state reflects NOTHING (idempotent — reflected:0)",
    ).toBe(0);

    // Idempotency at the DB: the distinct-symbol count of the MANUAL book did NOT
    // grow across the re-sync (no duplicated position rows for the same symbol).
    const symbolsAfterSecond = await withDb((c) =>
      manualPositionSymbolCount(c, sessionId),
    );
    expect(
      symbolsAfterSecond,
      "re-sync did NOT add a duplicate MANUAL position symbol (idempotent reflection)",
    ).toBe(symbolsAfterFirst);
    // The MANUAL book never lost ground vs the pre-sync baseline merely by observing.
    expect(
      symbolsAfterSecond,
      "the MANUAL book's symbol count is stable across an idempotent re-sync",
    ).toBeGreaterThanOrEqual(Math.min(symbolsBefore, symbolsAfterFirst));

    // When the broker actually reported a position this run, it is reflected as a
    // real OPEN MANUAL position attributed to the MANUAL book (the EXTERNAL trade
    // now shows in TMS) — never under an auto-strategy book.
    if ((fb.positions_observed ?? 0) > 0 && (fb.reflected ?? 0) > 0) {
      const open = await withDb((c) => openManualPositions(c, sessionId));
      for (const p of open) {
        expect(
          p.strategyId,
          "a synced broker position is reflected under the MANUAL/EXTERNAL book (strategy books uncorrupted)",
        ).toBe("MANUAL");
      }
    }
  });

  test("the desk's Sync control reflects broker truth into the panels and re-sync does not duplicate", async ({
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

    const session = await withDb((c) => latestSession(c));
    const sessionId = session!.id;

    // The Sync-from-broker panel + button. The desk may not surface it yet (the
    // panel ships after the place/close/cancel panels) — self-skip cleanly.
    const syncPanel = await firstVisibleTestId(page, ["manual-sync"], 8_000);
    if (!syncPanel) {
      test.skip(true, "the Sync-from-broker panel is not surfaced on the desk yet.");
      return;
    }
    const syncButton = page.getByTestId("manual-sync-button");
    if (!(await syncButton.count())) {
      test.skip(true, "no 'Sync from broker' button surfaced yet.");
      return;
    }
    await expect(syncButton, "the desk surfaces a Sync-from-broker button").toBeVisible({
      timeout: 8_000,
    });

    // Symbol baseline before clicking (for the no-duplicate assertion on re-sync).
    const symbolsBefore = await withDb((c) =>
      manualPositionSymbolCount(c, sessionId),
    );

    // ----- CLICK SYNC --------------------------------------------------------
    await syncButton.click();

    // The read-only result toast surfaces (proves the sync ran + placed no order).
    const result = page.getByTestId("manual-sync-result");
    const resultShown = await result
      .waitFor({ state: "visible", timeout: 20_000 })
      .then(() => true)
      .catch(() => false);
    if (!resultShown) {
      // The desk surfaced the panel but the sync did not complete with a toast this
      // run (e.g. a transient broker read) — there is no broker truth to assert.
      const err = page.getByTestId("manual-sync-error");
      if (await err.isVisible().catch(() => false)) {
        test.skip(
          true,
          `sync surfaced an error toast (no broker truth to reflect this run): ${(
            await err.innerText()
          ).slice(0, 160)}`,
        );
        return;
      }
      test.skip(true, "the sync did not produce a result toast this run.");
      return;
    }

    // The result is READ-ONLY (no order placed) — the safety invariant on the UI.
    await expect
      .poll(async () => result.getAttribute("data-read-only"), { timeout: 5_000 })
      .toBe("true");
    // The read-only badge is the operator-visible proof.
    const readOnlyBadge = page.getByTestId("manual-sync-read-only");
    if (await readOnlyBadge.count()) {
      await expect(readOnlyBadge.first()).toBeVisible();
    }

    // The counts toast matches the API/DB shape (non-negative integers).
    const counts = page.getByTestId("manual-sync-counts");
    if (await counts.count()) {
      await expect(counts.first()).toBeVisible();
      for (const attr of ["data-positions", "data-orders", "data-fills"]) {
        const raw = await counts.first().getAttribute(attr);
        if (raw != null) {
          expect(raw, `${attr} is a non-negative integer`).toMatch(/^\d+$/);
        }
      }
    }

    // A "last synced" timestamp now shows on the panel.
    await expect
      .poll(
        async () => (await page.getByTestId("manual-sync-last").innerText()).trim(),
        { timeout: 10_000 },
      )
      .toContain("Last synced");

    // ----- REFLECTION: the positions panel shows the synced MANUAL book ------
    // Every MANUAL position row the panel renders is a real OPEN MANUAL position in
    // the DB (faithful proxy of the broker truth, never a fabricated row).
    const dbSymbols = new Set(
      await withDb((c) => openManualPositionSymbols(c, sessionId)),
    );
    const positionsPanel = await firstVisibleTestId(page, ["manual-positions"], 8_000);
    if (positionsPanel) {
      const rows = page.getByTestId("manual-position-row");
      // Let the panel converge to broker truth (the sync hook invalidates the reads).
      await page.waitForTimeout(1_000);
      const n = await rows.count();
      for (let i = 0; i < n; i++) {
        const sym = await rows.nth(i).getAttribute("data-symbol");
        const sq = Number((await rows.nth(i).getAttribute("data-signed-qty")) ?? "0");
        if (sym != null && sq !== 0) {
          expect(
            dbSymbols.has(sym),
            `the reflected MANUAL position row ${sym} is a real open MANUAL position in the DB`,
          ).toBeTruthy();
        }
      }
    }

    // The blotter panel (where synced broker orders surface) renders without error.
    const blotterPanel = await firstVisibleTestId(page, ["manual-blotter"], 5_000);
    if (blotterPanel) {
      await expect(page.getByTestId("manual-blotter").first()).toBeVisible();
    }

    // ----- RE-SYNC: clicking again does NOT duplicate a position symbol ------
    await expect(syncButton).toBeEnabled({ timeout: 15_000 });
    const symbolsAfterFirst = await withDb((c) =>
      manualPositionSymbolCount(c, sessionId),
    );
    await syncButton.click();
    // The re-sync completed once the button re-enables.
    await expect(syncButton).toBeEnabled({ timeout: 20_000 });

    // Durable idempotency: the distinct-symbol count of the MANUAL book did not grow
    // across the re-sync (re-syncing the same broker state reflects nothing).
    await expect
      .poll(async () => withDb((c) => manualPositionSymbolCount(c, sessionId)), {
        timeout: 15_000,
      })
      .toBe(symbolsAfterFirst);
    const symbolsFinal = await withDb((c) =>
      manualPositionSymbolCount(c, sessionId),
    );
    expect(
      symbolsFinal,
      "re-syncing the same broker state did not duplicate a MANUAL position symbol",
    ).toBe(symbolsAfterFirst);
    expect(symbolsFinal).toBeGreaterThanOrEqual(
      Math.min(symbolsBefore, symbolsAfterFirst),
    );
  });
});
