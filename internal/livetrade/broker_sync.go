package livetrade

// broker_sync.go is DIRECTION 2 — the broker -> TMS SYNC/REFLECT flow (the user's
// PRIMARY case). The operator trades DIRECTLY in the moomoo app (NO order placed via
// TMS); they then return to TMS and click "Sync from broker". SyncFromBroker pulls
// the account's ACTUAL state (Trd_GetPositionList + Trd_GetOrderList +
// Trd_GetOrderFillList + Trd_GetFunds) and REFLECTS it into TMS so
// positions/orders/fills/account show what was done in moomoo.
//
// SAFETY (paramount): the sync is READ-ONLY from the broker. The executor primitive
// (SyncBrokerInto) calls ONLY the Trd_Get* reads — it NEVER calls PlaceOrder — so a
// sync can NOT place a real order and is safe in ALL modes, INCLUDING signal mode (a
// signal-mode operator can sync to see/manage what they actually hold). Nothing
// crosses the wire to the venue.
//
// ATTRIBUTION: the synced broker positions are reflected under the EXTERNAL pseudo-
// strategy id, distinct from the auto strategies' books, so externally-placed trades
// surface in TMS WITHOUT corrupting the strategy books. The synced broker truth is
// then reconciled (P6 reconciliation: broker net vs the strategy books) so drift the
// external trades introduced is reported to live.reconciliation_reports.
//
// IDEMPOTENCY: SyncBrokerInto reflects the DELTA between the broker net and the
// EXTERNAL book net per symbol, so re-syncing the SAME broker state reflects nothing
// (the book already equals the broker) and writes no duplicate rows.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/domain"
	moexec "github.com/byjackchen/trade-tms-go/internal/exec/moomoo"
	"github.com/byjackchen/trade-tms-go/internal/riskgate"
)

// SyncReport is what a SyncFromBroker observed + changed. Counts come from the
// broker pull; Reconciliation is the P6 report (broker vs strategy books) when a
// reconciler is wired (HasReconciliation reports whether it ran).
type SyncReport struct {
	// PositionsObserved / OrdersObserved / FillsObserved are the broker row counts
	// pulled READ-ONLY (the ACTUAL account state).
	PositionsObserved int
	OrdersObserved    int
	FillsObserved     int
	// Reflected is how many symbols' EXTERNAL book net was moved to match the
	// broker (a non-zero delta settled). Zero on a clean re-sync of the same state.
	Reflected int
	// Funds is the broker's account/buying-power snapshot at sync time.
	Funds moexec.BrokerSnapshot
	// HasReconciliation reports whether the reconciliation step ran.
	HasReconciliation bool
	// Reconciliation is the drift report (broker truth vs strategy books). Valid only
	// when HasReconciliation. Its HasIssues()/Summary() surface any drift.
	Reconciliation riskgate.ReconciliationReport
	// TS is the sync timestamp.
	TS time.Time
}

// HasDrift reports whether the reconciliation step ran AND found drift.
func (r SyncReport) HasDrift() bool {
	return r.HasReconciliation && r.Reconciliation.HasIssues()
}

// SyncFromBroker is DIRECTION 2: it pulls the account's ACTUAL state from the broker
// (READ-ONLY) and REFLECTS it into TMS under the EXTERNAL book, then runs the
// existing reconciliation (broker vs strategy books) so externally-introduced drift
// is reported. It places NO orders and is safe in ALL modes, including signal mode.
//
// operator identifies the human (-> audit). The returned SyncReport carries the
// observed counts + the reconciliation result.
//
// Flow:
//  1. reflect the broker truth into the EXTERNAL book (SyncBrokerInto — read-only pull
//     + local synthetic-fill settlement; idempotent on re-sync);
//  2. audit the sync action (ops.audit_log — operator, action "sync", ts);
//  3. run the P6 reconciliation (broker net vs strategy books -> reconciliation_reports)
//     when a reconciler is wired.
func (m *BrokerSyncController) SyncFromBroker(ctx context.Context, operator string) (SyncReport, error) {
	if strings.TrimSpace(operator) == "" {
		return SyncReport{}, fmt.Errorf("%w: sync requires an operator", domain.ErrInvalidArgument)
	}
	ts := m.clock()

	// (1) read-only pull + reflect into the EXTERNAL book. NO order is placed.
	syncRes, err := m.exec.SyncBrokerInto(ctx, ExternalStrategyID)
	if err != nil {
		// Still audit the attempted sync (a durable record of the operator action).
		m.audit0(ctx, SyncAuditRecord{
			Operator: operator,
			Action:   "sync",
			Live:     m.IsLive(),
			TS:       ts,
		})
		return SyncReport{}, fmt.Errorf("broker sync: %w", err)
	}

	report := SyncReport{
		PositionsObserved: syncRes.PositionsObserved,
		OrdersObserved:    syncRes.OrdersObserved,
		FillsObserved:     syncRes.FillsObserved,
		Reflected:         syncRes.Reflected,
		Funds:             syncRes.Snapshot,
		TS:                ts,
	}

	// (2) audit the sync (operator/ts; qty carries the reflected-symbol count so the
	// audit trail shows the magnitude of the reflection).
	m.audit0(ctx, SyncAuditRecord{
		Operator: operator,
		Action:   "sync",
		Qty:      int64(syncRes.Reflected),
		Live:     m.IsLive(),
		TS:       ts,
	})

	// (3) reconcile the broker truth against the strategy books (P6). The reconciler
	// pulls the broker positions itself + aggregates the books, persists the report
	// to reconciliation_reports + alerts on drift (never auto-trades).
	if m.reconciler != nil {
		rep, rerr := m.reconciler.Reconcile(ctx)
		if rerr != nil {
			return report, fmt.Errorf("broker sync: reconcile: %w", rerr)
		}
		report.HasReconciliation = true
		report.Reconciliation = rep
	}
	return report, nil
}
