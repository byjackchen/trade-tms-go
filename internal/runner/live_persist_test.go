package runner_test

// live_persist_test.go exercises the paper/live PG durability layer against a
// real database (ephemeral PG harness): orders/fills/positions upserts, gate
// risk-events, reconciliation reports, and strategy-state save/load round-trip.
// These cannot be faked — the schema CHECK constraints + the ON CONFLICT
// idempotency are only proven against PostgreSQL.

import (
	"testing"
	"time"

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
