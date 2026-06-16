//go:build integration

package apistore

// trade_store_test.go validates the paper/live trading READ surface
// (TradeStore.RecentOrders/RecentFills/OpenPositions/LatestReconciliation)
// against the real schema: it inserts trading rows directly and asserts the
// store returns them with correct 1e-4 fixed-point -> float conversion.

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/testutil/pgtest"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m, "apistore")) }

func TestLiveStoreTradingReads(t *testing.T) {
	itestPool := pgtest.RequirePG(t)
	ctx := context.Background()
	_, err := itestPool.Exec(ctx, `TRUNCATE tms.sessions, tms.orders, tms.fills, tms.positions,
		tms.reconciliation_reports RESTART IDENTITY CASCADE`)
	require.NoError(t, err)

	var sessionID int64
	require.NoError(t, itestPool.QueryRow(ctx,
		`INSERT INTO tms.sessions (trader_id, mode, status) VALUES ('PAPER-READ-001','paper','RUNNING') RETURNING id`).
		Scan(&sessionID))

	// One filled order + its fill + position.
	var orderID int64
	require.NoError(t, itestPool.QueryRow(ctx, `
		INSERT INTO tms.orders (session_id, client_order_id, venue_order_id, strategy_id, symbol,
		    instrument_id, side, order_type, qty, filled_qty, avg_fill_px, status, ts_last_event)
		VALUES ($1,'O-1','V-1','TEST-001','AAPL','AAPL.MOOMOO','BUY','MARKET',100,100,1500000,'FILLED',now())
		RETURNING id`, sessionID).Scan(&orderID))
	_, err = itestPool.Exec(ctx, `
		INSERT INTO tms.fills (order_id, venue_trade_id, qty, px, fee_usd, ts)
		VALUES ($1,'V-1-0',100,1500000,0,now())`, orderID)
	require.NoError(t, err)
	_, err = itestPool.Exec(ctx, `
		INSERT INTO tms.positions (session_id, position_id, strategy_id, symbol, instrument_id,
		    signed_qty, avg_entry_px, realized_pnl_usd, status, opened_at)
		VALUES ($1,'TEST-001|AAPL','TEST-001','AAPL','AAPL.MOOMOO',100,1500000,0,'OPEN',now())`, sessionID)
	require.NoError(t, err)
	_, err = itestPool.Exec(ctx, `
		INSERT INTO tms.reconciliation_reports (session_id, ts, tolerance_shares, matched,
		    mismatches, symbols_only_in_strategies, symbols_only_at_broker)
		VALUES ($1, now(), 0, '{AAPL}', '[]'::jsonb, '{}', '{}')`, sessionID)
	require.NoError(t, err)

	store := NewTradeStore(itestPool)

	orders, err := store.RecentOrders(ctx, "", 10)
	require.NoError(t, err)
	require.Len(t, orders, 1)
	assert.Equal(t, "O-1", orders[0].ClientOrderID)
	assert.Equal(t, int64(100), orders[0].FilledQty)
	assert.InDelta(t, 150.0, orders[0].AvgFillPx, 1e-9, "1e-4 fixed-point -> 150.00")
	assert.Equal(t, "FILLED", orders[0].Status)

	fills, err := store.RecentFills(ctx, "AAPL", 10)
	require.NoError(t, err)
	require.Len(t, fills, 1)
	assert.Equal(t, "V-1-0", fills[0].TradeID)
	assert.InDelta(t, 150.0, fills[0].Price, 1e-9)

	positions, err := store.OpenPositions(ctx)
	require.NoError(t, err)
	require.Len(t, positions, 1)
	assert.Equal(t, int64(100), positions[0].SignedQty)
	assert.InDelta(t, 150.0, positions[0].AvgEntryPx, 1e-9)

	recon, err := store.LatestReconciliation(ctx)
	require.NoError(t, err)
	require.NotNil(t, recon)
	assert.False(t, recon.HasIssues)
	assert.Equal(t, []string{"AAPL"}, recon.Matched)
}

// TestSessionRealizedPnLIncludesClosed locks the P7 day-P/L regression: realized
// PnL from a position closed intraday (e.g. a rebalance dropping a sector) must
// be included in day P/L even though OpenPositions excludes it. Before the fix,
// handleLiveAccount summed realized over OpenPositions only and reported $0.
func TestSessionRealizedPnLIncludesClosed(t *testing.T) {
	itestPool := pgtest.RequirePG(t)
	ctx := context.Background()
	_, err := itestPool.Exec(ctx, `TRUNCATE tms.sessions, tms.positions RESTART IDENTITY CASCADE`)
	require.NoError(t, err)

	var sessionID int64
	require.NoError(t, itestPool.QueryRow(ctx,
		`INSERT INTO tms.sessions (trader_id, mode, status) VALUES ('PAPER-PNL-001','paper','RUNNING') RETURNING id`).
		Scan(&sessionID))

	// An OPEN position with +$120.00 realized and a CLOSED (flat) position with
	// -$859.74 realized (closed by a rebalance). Net day P/L = -$739.74.
	_, err = itestPool.Exec(ctx, `
		INSERT INTO tms.positions (session_id, position_id, strategy_id, symbol, instrument_id,
		    signed_qty, avg_entry_px, realized_pnl_usd, status, opened_at, closed_at)
		VALUES ($1,'SECT|XLK','SECT','XLK','XLK.MOOMOO',288,2000000,1200000,'OPEN',now(),NULL),
		       ($1,'SECT|XLP','SECT','XLP','XLP.MOOMOO',0,3500000,-8597400,'CLOSED',now(),now())`, sessionID)
	require.NoError(t, err)

	store := NewTradeStore(itestPool)

	// OpenPositions excludes the closed XLP entirely.
	open, err := store.OpenPositions(ctx)
	require.NoError(t, err)
	require.Len(t, open, 1)
	assert.Equal(t, "XLK", open[0].Symbol)

	// SessionRealizedPnL must include BOTH: 120.00 + (-859.74) = -739.74.
	day, err := store.SessionRealizedPnL(ctx)
	require.NoError(t, err)
	assert.InDelta(t, -739.74, day, 1e-6, "day P/L must include intraday-closed realized PnL")
}
