//go:build integration

package runner_test

// live_persist_test.go exercises the paper/live PG durability layer against a
// real database (ephemeral PG harness): orders/fills/positions upserts, gate
// risk-events, reconciliation reports, and strategy-state save/load round-trip.
// These cannot be faked — the schema CHECK constraints + the ON CONFLICT
// idempotency are only proven against PostgreSQL.

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/commands"
	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/livetrade"
	"github.com/byjackchen/trade-tms-go/internal/portfolio"
	"github.com/byjackchen/trade-tms-go/internal/runner"
)

func TestLivePersistRoundTrip(t *testing.T) {
	pool := requirePG(t)
	ctx := testCtx(t)
	sessionID := openTestSession(t, pool, "PAPER-PERSIST-001")

	p := runner.NewLivePersist(pool, nil, sessionID, "PAPER-PERSIST-001", "MOOMOO", zerolog.Nop())

	// Order -> fill -> position round-trip.
	o := domain.NewMarketOrder("PAPER-O-1", "TEST-001", "AAPL", domain.OrderSideBuy, 100, "open", time.Now().UTC())
	o.Status = domain.OrderStatusSubmitted
	require.NoError(t, p.UpsertOrder(ctx, o))
	// Idempotent re-upsert (accepted).
	o.Status = domain.OrderStatusAccepted
	o.VenueOrderID = "V1"
	require.NoError(t, p.UpsertOrder(ctx, o))

	f := domain.Fill{
		TradeID: "V1-0", ClientOrderID: "PAPER-O-1", VenueOrderID: "V1",
		StrategyID: "TEST-001", Symbol: "AAPL", Side: domain.OrderSideBuy,
		Qty: 100, Price: domain.MustPrice("150.00"), TS: time.Now().UTC(),
	}
	require.NoError(t, p.InsertFill(ctx, f))
	// Duplicate fill is a no-op.
	require.NoError(t, p.InsertFill(ctx, f))

	pos := domain.Position{
		StrategyID: "TEST-001", Symbol: "AAPL", SignedQty: 100,
		AvgPx: domain.MustPrice("150.00"), UpdatedAt: time.Now().UTC(),
	}
	require.NoError(t, p.UpsertPosition(ctx, pos))

	var orderCount, fillCount, posQty int64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM tms.orders WHERE client_order_id='PAPER-O-1' AND status='FILLED'`).Scan(&orderCount))
	assert.Equal(t, int64(1), orderCount, "order rolled up to FILLED on the fill")
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM tms.fills`).Scan(&fillCount))
	assert.Equal(t, int64(1), fillCount, "duplicate fill deduped")
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT signed_qty FROM tms.positions WHERE strategy_id='TEST-001' AND symbol='AAPL'`).Scan(&posQty))
	assert.Equal(t, int64(100), posQty)

	// Gate decision -> risk_events.
	require.NoError(t, p.RecordGateDecision(ctx, livetrade.GateDecision{
		Approved: false, RuleName: "risk.daily_loss_halt", Reason: "halt active",
		StrategyID: "TEST-001", Symbol: "MSFT", Side: domain.SideLong, Qty: 10,
		Price: domain.MustPrice("50.00"), TS: time.Now().UTC(),
	}))
	var riskCount int64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM tms.risk_events WHERE rule_name='risk.daily_loss_halt' AND NOT approved`).Scan(&riskCount))
	assert.Equal(t, int64(1), riskCount)

	// Reconciliation report -> reconciliation_reports.
	report := portfolio.Reconcile(time.Now().UTC(),
		map[portfolio.PositionKey]int64{{StrategyID: "TEST-001", Symbol: "AAPL"}: 100},
		map[string]int64{"AAPL": 95}, 0)
	require.True(t, report.HasIssues())
	require.NoError(t, p.SaveReconciliation(ctx, report, 0))
	var hasIssues bool
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT has_issues FROM tms.reconciliation_reports ORDER BY id DESC LIMIT 1`).Scan(&hasIssues))
	assert.True(t, hasIssues, "mismatch report persisted with has_issues")

	// Strategy state save/load round-trip.
	require.NoError(t, p.SaveState(ctx, "TEST-001", []byte(`{"generation":7,"warm":true}`)))
	require.NoError(t, p.SaveState(ctx, "TEST-001", []byte(`{"generation":8,"warm":true}`)))
	state, ok, err := p.LoadState(ctx, "TEST-001")
	require.NoError(t, err)
	require.True(t, ok)
	assert.JSONEq(t, `{"generation":8,"warm":true}`, string(state), "latest state wins")
	var generation int64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT generation FROM tms.strategy_state WHERE strategy_id='TEST-001'`).Scan(&generation))
	assert.Equal(t, int64(1), generation, "generation bumps on re-save")

	// A strategy with no state loads ok=false.
	_, ok, err = p.LoadState(ctx, "UNKNOWN")
	require.NoError(t, err)
	assert.False(t, ok)
}

// TestUpsertOrderFilledCarriesFilledQty is the regression for the paper/live
// order-persistence defect: an EffectStatus(FILLED) snapshot must carry
// filled_qty=qty so UpsertOrder does NOT violate CHECK (status<>'FILLED' OR
// filled_qty=qty) (SQLSTATE 23514). It writes a FILLED order directly (no fill
// roll-up to mask it) and asserts the row lands with filled_qty=qty + avg_fill_px.
func TestUpsertOrderFilledCarriesFilledQty(t *testing.T) {
	pool := requirePG(t)
	ctx := testCtx(t)
	sessionID := openTestSession(t, pool, "PAPER-FILLED-001")
	p := runner.NewLivePersist(pool, nil, sessionID, "PAPER-FILLED-001", "MOOMOO", zerolog.Nop())

	// Mirror the executor's orderSnapshot on an EffectStatus(FILLED): the order is
	// FILLED with the cumulative fill carried on the snapshot itself.
	o := domain.NewMarketOrder("PAPER-O-F", "TEST-001", "AAPL", domain.OrderSideBuy, 100, "open", time.Now().UTC())
	o.Status = domain.OrderStatusFilled
	o.FilledQty = 100
	o.AvgFillPx = domain.MustPrice("150.25")
	require.NoError(t, o.Validate(), "a FILLED order with filled_qty=qty is valid in-domain")
	// This is the exact write that previously failed with orders_check3 (the bug
	// set status=FILLED, filled_qty=0). It must now succeed with no error.
	require.NoError(t, p.UpsertOrder(ctx, o), "FILLED order persists without violating orders_check3")

	var status string
	var filledQty, avgPx int64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status, filled_qty, avg_fill_px FROM tms.orders WHERE client_order_id='PAPER-O-F'`).
		Scan(&status, &filledQty, &avgPx))
	assert.Equal(t, "FILLED", status)
	assert.Equal(t, int64(100), filledQty, "filled_qty carried into the FILLED row")
	assert.Equal(t, int64(1502500), avgPx, "avg_fill_px persisted on the 1e-4 grid")

	// A partial-fill snapshot (filled_qty<qty) round-trips too, and filled_qty must
	// never regress on a later upsert (GREATEST guard).
	o2 := domain.NewMarketOrder("PAPER-O-P", "TEST-001", "AAPL", domain.OrderSideBuy, 100, "open", time.Now().UTC())
	o2.Status = domain.OrderStatusPartiallyFilled
	o2.FilledQty = 40
	o2.AvgFillPx = domain.MustPrice("150.00")
	require.NoError(t, p.UpsertOrder(ctx, o2))
	// A stale duplicate carrying a LOWER filled_qty must not walk the row backwards.
	o2.FilledQty = 10
	require.NoError(t, p.UpsertOrder(ctx, o2))
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT filled_qty FROM tms.orders WHERE client_order_id='PAPER-O-P'`).Scan(&filledQty))
	assert.Equal(t, int64(40), filledQty, "filled_qty never regresses on a stale duplicate")
}

// TestUpsertOrderPersistsTypeAndLimitPx is the regression for finding 2: a LIMIT
// order must persist with order_type='LIMIT' AND limit_px (USD fixed-point 1e-4),
// not the hardcoded 'MARKET' with a NULL limit_px the manual desk wrote before.
// The manual desk fully supports LIMIT (validated + sent to the venue with the
// limit price), so the durable record + blotter must faithfully reflect the
// operator's order type + limit price on this audited, real-money-capable surface.
func TestUpsertOrderPersistsTypeAndLimitPx(t *testing.T) {
	pool := requirePG(t)
	ctx := testCtx(t)
	sessionID := openTestSession(t, pool, "PAPER-LIMIT-001")
	p := runner.NewLivePersist(pool, nil, sessionID, "PAPER-LIMIT-001", "MOOMOO", zerolog.Nop())

	// A LIMIT BUY (MSFT @ 100.00). Build the order exactly as the executor's
	// SubmitManual does for a LIMIT spec: Type=LIMIT with a positive LimitPrice.
	lp := domain.MustPrice("100.00")
	o := domain.Order{
		ClientOrderID: "MANUAL-PAPER-lim-1", StrategyID: livetrade.ManualStrategyID,
		Symbol: "MSFT", Side: domain.OrderSideBuy, Type: domain.OrderTypeLimit,
		TIF: domain.TIFGTC, Qty: 5, LimitPrice: &lp,
		Status: domain.OrderStatusSubmitted, TS: time.Now().UTC(),
	}
	require.NoError(t, o.Validate(), "a LIMIT order with a positive limit price is valid in-domain")
	require.NoError(t, p.UpsertOrder(ctx, o), "LIMIT order persists (order_type CHECK satisfied)")

	var orderType string
	var limitPx int64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT order_type, limit_px FROM tms.orders WHERE client_order_id='MANUAL-PAPER-lim-1'`).
		Scan(&orderType, &limitPx))
	assert.Equal(t, "LIMIT", orderType, "the LIMIT order persists as LIMIT, not the old hardcoded MARKET")
	assert.Equal(t, int64(1000000), limitPx, "limit_px persists on the 1e-4 grid ($100.00 -> 1_000_000)")

	// A MARKET order persists order_type='MARKET' with a NULL limit_px (no ceiling).
	m := domain.NewMarketOrder("MANUAL-PAPER-mkt-1", livetrade.ManualStrategyID, "AAPL", domain.OrderSideBuy, 10, "open", time.Now().UTC())
	m.Status = domain.OrderStatusSubmitted
	require.NoError(t, p.UpsertOrder(ctx, m))
	var mType string
	var mLimit *int64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT order_type, limit_px FROM tms.orders WHERE client_order_id='MANUAL-PAPER-mkt-1'`).
		Scan(&mType, &mLimit))
	assert.Equal(t, "MARKET", mType)
	assert.Nil(t, mLimit, "a MARKET order carries no limit_px (NULL)")
}

// openPositionCount returns the number of OPEN tms.positions rows for the
// session — the cockpit's "open positions" gauge. After a flatten it MUST be 0.
func openPositionCount(t *testing.T, pool *pgxpool.Pool, sessionID int64) int64 {
	t.Helper()
	ctx := testCtx(t)
	var n int64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM tms.positions WHERE session_id=$1 AND status='OPEN'`, sessionID).Scan(&n))
	return n
}

// TestFlattenLeavesPositionBookFlatInPG is the persistence-level regression that
// the P6 gate MISSED: it asserts the open-position COUNT in tms.positions drops
// to 0 after a flatten — i.e. the originating '<strategy>|<sym>' row is stamped
// status=CLOSED (signed_qty 0) and NO phantom 'FLATTEN|<sym>' OPEN row is left
// behind. The old (buggy) flatten closed the broker aggregate under a FLATTEN
// position_id, so the book held PAIRED rows (the strategy row still OPEN at +N
// AND a FLATTEN row at -N): economically flat, but openPositionCount != 0 and
// the cockpit showed phantom open positions. The correct model nets each
// originating row to 0, so this count is 0.
func TestFlattenLeavesPositionBookFlatInPG(t *testing.T) {
	pool := requirePG(t)
	ctx := testCtx(t)
	sessionID := openTestSession(t, pool, "PAPER-FLATTEN-001")
	p := runner.NewLivePersist(pool, nil, sessionID, "PAPER-FLATTEN-001", "MOOMOO", zerolog.Nop())

	// Two strategies, one sharing a symbol — the multi-strategy-same-symbol case
	// from the diagnosis (SectorRotation-001/XLK + Hedge-002/XLK), plus a distinct
	// MSFT row. Open each (the executor's persistPosition writes the OPEN row).
	opens := []domain.Position{
		{StrategyID: "SectorRotation-001", Symbol: "XLK", SignedQty: 348, AvgPx: domain.MustPrice("200.00"), UpdatedAt: time.Now().UTC()},
		{StrategyID: "Hedge-002", Symbol: "XLK", SignedQty: -100, AvgPx: domain.MustPrice("200.00"), UpdatedAt: time.Now().UTC()},
		{StrategyID: "SectorRotation-001", Symbol: "MSFT", SignedQty: -50, AvgPx: domain.MustPrice("300.00"), UpdatedAt: time.Now().UTC()},
	}
	for _, o := range opens {
		require.NoError(t, p.UpsertPosition(ctx, o))
	}
	require.Equal(t, int64(3), openPositionCount(t, pool, sessionID), "3 open rows before flatten")

	// CORRECT-MODEL flatten: each originating row's closing fill nets it to 0, so
	// the executor's persistPosition writes the SAME (strategy, symbol) row at
	// signed_qty 0 -> UpsertPosition stamps status=CLOSED. This is the exact write
	// the rewritten Flatten produces (close under the ORIGINATING strategy id,
	// NOT a FLATTEN pseudo-strategy).
	for _, o := range opens {
		closed := o
		closed.SignedQty = 0
		closed.UpdatedAt = time.Now().UTC()
		require.NoError(t, p.UpsertPosition(ctx, closed))
	}

	// THE STRENGTHENED ASSERTION (the gap P6 missed): the open-position COUNT is 0.
	assert.Equal(t, int64(0), openPositionCount(t, pool, sessionID),
		"after a flatten the position BOOK must be row-by-row flat (no phantom OPEN rows)")

	// Each originating row is CLOSED (not a NEW FLATTEN row): same position_id,
	// status flipped to CLOSED, closed_at stamped.
	var closedCount int64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM tms.positions
		  WHERE session_id=$1 AND status='CLOSED' AND closed_at IS NOT NULL`, sessionID).Scan(&closedCount))
	assert.Equal(t, int64(3), closedCount, "each originating row is stamped CLOSED in place")

	// And there is NO phantom FLATTEN-strategy row at all (old-model artifact).
	var flattenRows int64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM tms.positions WHERE session_id=$1 AND strategy_id='FLATTEN'`, sessionID).Scan(&flattenRows))
	assert.Equal(t, int64(0), flattenRows, "the correct model creates NO phantom FLATTEN position rows")
}

// TestFlattenConfirmationGate proves flatten + emergency_kill require a confirm
// token at enqueue (P6 decision 5/7), reconcile does not.
func TestFlattenConfirmationGate(t *testing.T) {
	pool := requirePG(t)
	ctx := testCtx(t)
	enq := commands.NewEnqueuer(pool, nil, "")

	// flatten without a token is rejected.
	_, err := enq.Enqueue(ctx, commands.EnqueueParams{Name: commands.NameFlatten, RequestedBy: "t"})
	require.ErrorIs(t, err, commands.ErrConfirmationRequired)

	// flatten WITH a token is accepted.
	_, err = enq.Enqueue(ctx, commands.EnqueueParams{
		Name: commands.NameFlatten, Args: commands.CommandArgs{ConfirmToken: "yes"}, RequestedBy: "t"})
	require.NoError(t, err)

	// emergency_kill without a token is rejected.
	_, err = enq.Enqueue(ctx, commands.EnqueueParams{Name: commands.NameEmergencyKill, RequestedBy: "t"})
	require.ErrorIs(t, err, commands.ErrConfirmationRequired)

	// reconcile needs no token (read-only).
	_, err = enq.Enqueue(ctx, commands.EnqueueParams{Name: commands.NameReconcile, RequestedBy: "t"})
	require.NoError(t, err)
}
