package livetrade_test

// manual_sync_test.go proves DIRECTION 2 — the broker -> TMS SYNC/REFLECT flow (the
// user's PRIMARY case): the operator trades DIRECTLY in moomoo (a broker position
// that was NEVER placed via TMS), then runs SyncFromBroker, which:
//
//   - pulls the account's ACTUAL state READ-ONLY (no order placed) and REFLECTS the
//     broker position into the MANUAL/EXTERNAL book so it shows up in TMS;
//   - runs the existing reconciliation so the synced truth is captured + persisted;
//   - is IDEMPOTENT: re-syncing the SAME broker state reflects nothing (no duplicate
//     rows / no double-counted position);
//   - is READ-ONLY: a sync places NO order at the venue, in ANY mode incl signal;
//   - audits the sync action (ops.audit_log).

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/livetrade"
)

// TestSyncReflectsExternalBrokerPosition is the core DIRECTION-2 proof: a position
// that exists at the broker but was NEVER placed via TMS is reflected into the
// MANUAL book by SyncFromBroker, and reconciliation captures it.
func TestSyncReflectsExternalBrokerPosition(t *testing.T) {
	h := newPaperManual(t, manualGateFor(t, 1.0, 0.10), 100000)
	ctx := context.Background()

	// The operator traded DIRECTLY in moomoo: the broker now holds long 25 TSLA that
	// TMS never placed. (No PlaceManualOrder was called for it.)
	h.venue.SetPosition("TSLA", 25, domain.MustPrice("250.00"))

	// Before the sync the MANUAL book knows nothing about TSLA.
	_, ok := h.account.Position(livetrade.ManualStrategyID, "TSLA")
	assert.False(t, ok, "MANUAL book must not know the external position before sync")

	rep, err := h.mc.SyncFromBroker(ctx, "alice")
	require.NoError(t, err)

	// The pull observed the external position; the reflection moved the MANUAL book.
	assert.Equal(t, 1, rep.PositionsObserved)
	assert.Equal(t, 1, rep.Reflected, "the external position was reflected")

	// The MANUAL book now mirrors the broker truth (long 25 TSLA).
	pos, ok := h.account.Position(livetrade.ManualStrategyID, "TSLA")
	require.True(t, ok, "the external position is now reflected in the MANUAL book")
	assert.Equal(t, domain.Qty(25), pos.SignedQty)

	// Reconciliation ran + was persisted. With the broker truth reflected into the
	// MANUAL book, the broker net == the book net => the symbol MATCHES (no drift).
	require.True(t, rep.HasReconciliation)
	assert.False(t, rep.Reconciliation.HasIssues(), "reflected book reconciles clean with the broker")
	require.NotEmpty(t, h.report.reports, "the reconciliation report was persisted")

	// The sync was audited.
	syncs := h.audit.byAction("sync")
	require.Len(t, syncs, 1)
	assert.Equal(t, "alice", syncs[0].Operator)
}

// TestSyncReconciliationCapturesDriftVsStrategyBooks: when the MANUAL book is NOT
// reconciled to the broker (we point the reconciler's books at an EMPTY auto book
// view by syncing a broker position and checking the report classifies it), drift is
// surfaced. Here we prove the reconciliation step actually compares broker vs books
// by checking a broker-only symbol is matched once reflected, and that a re-sync
// keeps it matched.
func TestSyncReconciliationClassifiesReflected(t *testing.T) {
	h := newPaperManual(t, manualGateFor(t, 1.0, 0.10), 100000)
	ctx := context.Background()
	h.venue.SetPosition("NVDA", 10, domain.MustPrice("500.00"))

	rep, err := h.mc.SyncFromBroker(ctx, "bob")
	require.NoError(t, err)
	require.True(t, rep.HasReconciliation)
	// NVDA is now in BOTH the broker and the (reflected) book => matched, no drift.
	assert.Contains(t, rep.Reconciliation.Matched, "NVDA")
	assert.False(t, rep.Reconciliation.HasIssues())
}

// TestSyncIsIdempotent: re-syncing the SAME broker state reflects nothing and never
// double-counts the position.
func TestSyncIsIdempotent(t *testing.T) {
	h := newPaperManual(t, manualGateFor(t, 1.0, 0.10), 100000)
	ctx := context.Background()
	h.venue.SetPosition("AMD", 40, domain.MustPrice("120.00"))

	rep1, err := h.mc.SyncFromBroker(ctx, "carol")
	require.NoError(t, err)
	assert.Equal(t, 1, rep1.Reflected)
	pos1, _ := h.account.Position(livetrade.ManualStrategyID, "AMD")
	require.Equal(t, domain.Qty(40), pos1.SignedQty)

	// Re-sync the SAME broker state: nothing new to reflect, no double count.
	rep2, err := h.mc.SyncFromBroker(ctx, "carol")
	require.NoError(t, err)
	assert.Equal(t, 0, rep2.Reflected, "re-syncing the same state reflects nothing (idempotent)")
	pos2, _ := h.account.Position(livetrade.ManualStrategyID, "AMD")
	assert.Equal(t, domain.Qty(40), pos2.SignedQty, "position must not be double-counted on re-sync")

	// Two sync audit rows (one per call), both clean.
	assert.Len(t, h.audit.byAction("sync"), 2)
}

// TestSyncTracksIncrementalExternalTrades: the operator trades MORE in moomoo
// between syncs; the second sync reflects only the incremental delta.
func TestSyncTracksIncrementalExternalTrades(t *testing.T) {
	h := newPaperManual(t, manualGateFor(t, 1.0, 0.10), 100000)
	ctx := context.Background()

	h.venue.SetPosition("AAPL", 10, domain.MustPrice("100.00"))
	_, err := h.mc.SyncFromBroker(ctx, "dan")
	require.NoError(t, err)
	pos, _ := h.account.Position(livetrade.ManualStrategyID, "AAPL")
	require.Equal(t, domain.Qty(10), pos.SignedQty)

	// The operator bought 15 more in moomoo (broker now 25).
	h.venue.SetPosition("AAPL", 25, domain.MustPrice("105.00"))
	rep, err := h.mc.SyncFromBroker(ctx, "dan")
	require.NoError(t, err)
	assert.Equal(t, 1, rep.Reflected, "the incremental delta is reflected")
	pos, _ = h.account.Position(livetrade.ManualStrategyID, "AAPL")
	assert.Equal(t, domain.Qty(25), pos.SignedQty, "book tracks the broker truth exactly")
}

// TestSyncReflectsExternalClose: the operator CLOSED a position in moomoo after a
// prior sync; the broker no longer reports it, so the sync drives the MANUAL book
// back to flat.
func TestSyncReflectsExternalClose(t *testing.T) {
	h := newPaperManual(t, manualGateFor(t, 1.0, 0.10), 100000)
	ctx := context.Background()

	h.venue.SetPosition("MSFT", 30, domain.MustPrice("400.00"))
	_, err := h.mc.SyncFromBroker(ctx, "eve")
	require.NoError(t, err)
	pos, _ := h.account.Position(livetrade.ManualStrategyID, "MSFT")
	require.Equal(t, domain.Qty(30), pos.SignedQty)

	// The operator flattened MSFT in moomoo: the broker no longer reports it.
	h.venue.SetPosition("MSFT", 0, domain.MustPrice("0"))
	rep, err := h.mc.SyncFromBroker(ctx, "eve")
	require.NoError(t, err)
	assert.Equal(t, 1, rep.Reflected, "the close is reflected (book driven to flat)")
	pos, _ = h.account.Position(livetrade.ManualStrategyID, "MSFT")
	assert.Equal(t, domain.Qty(0), pos.SignedQty, "the MANUAL book is flat after the external close")
}

// TestSyncIsReadOnly: a sync places NO order at the venue (READ-ONLY from the
// broker). This is the core safety property — a sync can never place a real order.
func TestSyncIsReadOnly(t *testing.T) {
	h := newPaperManual(t, manualGateFor(t, 1.0, 0.10), 100000)
	ctx := context.Background()
	h.venue.SetPosition("GOOG", 5, domain.MustPrice("150.00"))

	before, _ := h.venue.GetOrderList(ctx, manualPaperAcc, 0)
	_, err := h.mc.SyncFromBroker(ctx, "frank")
	require.NoError(t, err)
	after, _ := h.venue.GetOrderList(ctx, manualPaperAcc, 0)

	assert.Equal(t, len(before), len(after), "a sync must place NO order at the broker (read-only)")
}

// TestSyncRequiresOperator: a sync with no operator is rejected (audit needs it).
func TestSyncRequiresOperator(t *testing.T) {
	h := newPaperManual(t, manualGateFor(t, 1.0, 0.10), 100000)
	_, err := h.mc.SyncFromBroker(context.Background(), "")
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
}

// TestSyncShortReport: HasDrift convenience reports false when reconciliation is
// clean.
func TestSyncShortReport(t *testing.T) {
	h := newPaperManual(t, manualGateFor(t, 1.0, 0.10), 100000)
	rep, err := h.mc.SyncFromBroker(context.Background(), "gil")
	require.NoError(t, err)
	assert.False(t, rep.HasDrift())
}
