//go:build integration

package runs_test

// store_pg_test.go exercises the runs.Store against a REAL PostgreSQL /
// TimescaleDB (the equity_curves hypertable, FK cascades and partial indexes
// cannot be faked). The ephemeral-container bootstrap lives in
// internal/testutil/pgtest (shared with runner/jobs/study/api), skipping when
// docker is unavailable (TMS_TEST_NO_DOCKER=1).

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/metrics"
	"github.com/byjackchen/trade-tms-go/internal/runs"
	"github.com/byjackchen/trade-tms-go/internal/testutil/pgtest"
)

// testPool is the shared ephemeral pool, populated by the first requirePG call.
var testPool *pgxpool.Pool

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m, "runs")) }

// requirePG skips when the ephemeral DB is unavailable and exposes the shared
// pool via the package-local testPool. These tests share one database and must
// not run in parallel.
func requirePG(t *testing.T) {
	t.Helper()
	testPool = pgtest.RequirePG(t)
}

func samplePersist() runs.PersistInput {
	t0 := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	exitTS := time.Date(2024, 1, 4, 0, 0, 0, 0, time.UTC)
	exitPx := domain.MustPrice("12.00")
	pnl := domain.MustMoney("200.00")
	pm := metrics.BacktestMetrics{
		FinalBalanceUSD: 100200, TotalPnLUSD: 200, Sharpe: 1.2, Calmar: 2.4,
		MaxDrawdownPct: -1.5, NumOrders: 2, NumFilledOrders: 2, NumPositions: 1,
	}
	return runs.PersistInput{
		RunTS:            "2024-06-13_12-00-00",
		Kind:             "smoke",
		Status:           "COMPLETE",
		StartDate:        calendar.NewDate(2024, 1, 2),
		EndDate:          calendar.NewDate(2024, 1, 4),
		StartingBalance:  domain.MustMoney("100000.00"),
		FinalBalance:     domain.MustMoney("100200.00"),
		TotalPnL:         domain.MustMoney("200.00"),
		Strategies:       []string{"Scripted-000"},
		Config:           json.RawMessage(`{"start":"2024-01-02"}`),
		PortfolioMetrics: pm,
		StrategyMetrics:  map[string]metrics.BacktestMetrics{"Scripted-000": pm},
		PortfolioEquity: []runs.EquityPoint{
			{TS: t0, BalanceUSD: domain.MustMoney("100000.00")},
			{TS: exitTS, BalanceUSD: domain.MustMoney("100200.00")},
		},
		StrategyEquity: map[string][]runs.EquityPoint{
			"Scripted-000": {{TS: exitTS, BalanceUSD: domain.MustMoney("200.00")}},
		},
		Trades: []runs.Trade{{
			StrategyID: "Scripted-000", Symbol: "AAPL", Side: "LONG", Qty: 100,
			EntryTS: t0, ExitTS: &exitTS, EntryPx: domain.MustPrice("10.00"),
			ExitPx: &exitPx, RealizedPnL: pnl,
		}},
		Orders: []domain.Order{
			domain.NewMarketOrder("c1", "Scripted-000", "AAPL", domain.OrderSideBuy, 100, "buy", t0),
		},
	}
}

func TestStorePersistAndRead(t *testing.T) {
	requirePG(t)
	ctx := context.Background()
	store := runs.NewStore(testPool)

	id, err := store.Persist(ctx, samplePersist())
	require.NoError(t, err)
	require.Positive(t, id)

	// Detail.
	d, err := store.Get(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "2024-06-13_12-00-00", d.RunTS)
	assert.Equal(t, "COMPLETE", d.Status)
	require.NotNil(t, d.FinalBalance)
	assert.Equal(t, domain.MustMoney("100200.00"), *d.FinalBalance)
	require.NotNil(t, d.PortfolioMetrics)
	assert.Equal(t, 1.2, d.PortfolioMetrics.Sharpe)
	assert.Equal(t, 100200.0, d.PortfolioMetrics.FinalBalanceUSD)
	assert.Contains(t, d.StrategyMetrics, "Scripted-000")

	// List.
	list, err := store.List(ctx, runs.ListFilter{})
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, id, list[0].ID)

	// Equity (portfolio + strategy).
	pe, err := store.Equity(ctx, id, "")
	require.NoError(t, err)
	require.Len(t, pe, 2)
	assert.True(t, pe[0].TS.Before(pe[1].TS)) // ascending
	se, err := store.Equity(ctx, id, "Scripted-000")
	require.NoError(t, err)
	require.Len(t, se, 1)
	assert.Equal(t, domain.MustMoney("200.00"), se[0].BalanceUSD)

	// Trades.
	trs, err := store.Trades(ctx, id)
	require.NoError(t, err)
	require.Len(t, trs, 1)
	assert.Equal(t, "AAPL", trs[0].Symbol)
	require.NotNil(t, trs[0].RealizedPnL)
	assert.Equal(t, domain.MustMoney("200.00"), *trs[0].RealizedPnL)

	// Orders (from meta.orders).
	raw, err := store.Orders(ctx, id)
	require.NoError(t, err)
	var orders []map[string]any
	require.NoError(t, json.Unmarshal(raw, &orders))
	require.Len(t, orders, 1)
	assert.Equal(t, "c1", orders[0]["client_order_id"])
}

func TestStorePersistIdempotent(t *testing.T) {
	requirePG(t)
	ctx := context.Background()
	store := runs.NewStore(testPool)

	in := samplePersist()
	in.RunTS = "2024-06-13_13-00-00"
	id1, err := store.Persist(ctx, in)
	require.NoError(t, err)

	// Re-persist with the same run_ts: replaces, does not duplicate.
	id2, err := store.Persist(ctx, in)
	require.NoError(t, err)
	assert.NotEqual(t, id1, id2, "a replace produces a fresh identity row")

	// Only one run with that ts remains.
	var count int
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT count(*) FROM tms.runs WHERE run_ts = $1`, in.RunTS).Scan(&count))
	assert.Equal(t, 1, count)

	// Children of the replaced run were cascaded away.
	var trades int
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT count(*) FROM tms.trades WHERE run_id = $1`, id1).Scan(&trades))
	assert.Equal(t, 0, trades)
}

// TestStoreConcurrentSameSecondNoOverwrite is the DB-level regression for the
// round-3 data-loss blocker: two backtests that start within the same
// wall-clock second must BOTH persist. Previously both got the same
// second-resolution run_ts, and Persist's idempotent DELETE-then-INSERT on the
// UNIQUE(run_ts) natural key silently destroyed one run's rows + artifacts. With
// NewRunID the keys carry a microsecond+counter suffix, so both rows survive.
func TestStoreConcurrentSameSecondNoOverwrite(t *testing.T) {
	requirePG(t)
	ctx := context.Background()
	store := runs.NewStore(testPool)

	// The exact same wall-clock second for both runs (the collision condition).
	now := time.Date(2024, 7, 1, 2, 31, 59, 0, time.UTC)
	in1 := samplePersist()
	in1.RunTS = runs.NewRunID(now)
	in2 := samplePersist()
	in2.RunTS = runs.NewRunID(now)

	require.NotEqual(t, in1.RunTS, in2.RunTS, "NewRunID must be collision-free within a second")

	id1, err := store.Persist(ctx, in1)
	require.NoError(t, err)
	id2, err := store.Persist(ctx, in2)
	require.NoError(t, err)
	require.NotEqual(t, id1, id2)

	// BOTH runs are still readable — neither overwrote the other.
	d1, err := store.Get(ctx, id1)
	require.NoError(t, err, "first same-second run must survive")
	d2, err := store.Get(ctx, id2)
	require.NoError(t, err, "second same-second run must survive")
	assert.Equal(t, in1.RunTS, d1.RunTS)
	assert.Equal(t, in2.RunTS, d2.RunTS)

	// Both sub-second run_ts values satisfy the relaxed CHECK and both rows exist
	// for the shared second prefix.
	var count int
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT count(*) FROM tms.runs WHERE run_ts LIKE $1`, "2024-07-01_02-31-59%").Scan(&count))
	assert.Equal(t, 2, count, "both same-second runs persisted")
}

func TestStoreGetNotFound(t *testing.T) {
	requirePG(t)
	_, err := runs.NewStore(testPool).Get(context.Background(), 9_999_999)
	assert.ErrorIs(t, err, runs.ErrRunNotFound)
}

func TestStoreEquityNotFound(t *testing.T) {
	requirePG(t)
	_, err := runs.NewStore(testPool).Equity(context.Background(), 9_999_999, "")
	assert.ErrorIs(t, err, runs.ErrRunNotFound)
}
