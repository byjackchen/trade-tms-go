//go:build integration

package runner_test

// live_health_test.go covers the two fixer-round-2 findings against a REAL
// supervisor (runner.Live.Run) driven over the protocol-faithful mock OpenD and
// the ephemeral Postgres harness:
//
//	finding 1 — a `multi` (SEPA-bearing) node started with NO --tickers must
//	            resolve a default SF1 stock universe and run a working session,
//	            instead of hard-failing strategy assembly ("sepa needs at least
//	            one stock") and crash-looping forever.
//	finding 2 — when the node CANNOT keep a session running (empty universe,
//	            assembly fails on every restart), the inner SessionHealth goes
//	            unhealthy so /healthz reports degraded — a green liveness probe
//	            over a crash-looping session is a misleading production signal.
//
// Both are exercised end to end through runner.NewLive + Run (the same code the
// `tms live` command and the compose tms-live service run).

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/broker/moomoo/mock"
	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/runner"
)

// seedSF1Tickers registers each symbol as a tradable SF1 (common-stock) ticker
// with an open price window, so the assembler's default-universe resolver
// (ListUniverseForWindow table=SF1) returns them when --tickers is empty.
func seedSF1Tickers(t *testing.T, pool *pgxpool.Pool, symbols []string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, sym := range symbols {
		_, err := pool.Exec(ctx,
			`INSERT INTO tms.tickers (ticker, table_name, first_price_date, last_price_date)
			 VALUES ($1, 'SF1', NULL, NULL)
			 ON CONFLICT (ticker) DO UPDATE SET table_name = 'SF1'`,
			sym)
		require.NoError(t, err)
	}
}

// truncateTickers clears tms.tickers (requirePG truncates bars_daily + the live
// tables but NOT tickers; these tests seed/assert against it directly).
func truncateTickers(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := pool.Exec(ctx, `TRUNCATE tms.tickers CASCADE`)
	require.NoError(t, err)
}

// startMockOpenD starts a Postgres-backed mock OpenD (serves InitConnect /
// GlobalState / Sub / HistoryKL / UpdateKL from tms.bars_daily) and returns its
// dial address. The supervisor's moomoo client + warmup history requests resolve
// against it exactly as against real OpenD.
func startMockOpenD(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	srv, err := mock.New(mock.Options{Source: mock.NewPGBarSource(pool)})
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Close() })
	go func() { _ = srv.Serve(ctx) }()
	return srv.Addr()
}

// TestLiveDefaultUniverseRunsHealthy is finding 1 + the healthy half of finding
// 2: a `multi` node with NO --tickers, given a seeded SF1 universe + bars,
// resolves the default universe, opens a RUNNING session, and reports healthy on
// SessionHealth — it does NOT crash-loop.
func TestLiveDefaultUniverseRunsHealthy(t *testing.T) {
	pool := requirePG(t)
	truncateTickers(t, pool)
	ctx := testCtx(t)

	// Seed an SF1 stock universe + a long rising daily series (enough for SEPA
	// warmup to have history). SPY is needed for the multi context heartbeat.
	stocks := []string{"AAA", "BBB", "CCC"}
	seedSF1Tickers(t, pool, stocks)
	end := time.Now().UTC().Truncate(24 * time.Hour)
	dates := tradingDates(end, 60)
	seedDailyBars(t, pool, append(append([]string{}, stocks...), "SPY"), dates)

	addr := startMockOpenD(t, ctx, pool)

	node, err := runner.NewLive(pool, nil, runner.LiveConfig{
		TraderID:        "SIGNAL-DEFUNI",
		Mode:            "signal",
		Strategy:        "multi", // SEPA-bearing; NO Tickers supplied
		StartingBalance: 100000,
		MoomooAddr:      addr,
		BarSeconds:      86400,
	}, zerolog.Nop())
	require.NoError(t, err)

	// Run the supervisor in the background; it should reach a RUNNING session.
	runCtx, cancelRun := context.WithCancel(ctx)
	runErr := make(chan error, 1)
	go func() { runErr <- node.Run(runCtx) }()

	require.Eventually(t, func() bool {
		h := node.SessionHealth()
		return h.Running && h.Healthy && h.ConsecutiveFailures == 0
	}, 20*time.Second, 100*time.Millisecond,
		"multi node with no --tickers should resolve a default universe and run a healthy session")

	// A session row is RUNNING for this trader (the node opened it).
	var status string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status FROM tms.sessions WHERE trader_id=$1 ORDER BY started_at DESC LIMIT 1`,
		"SIGNAL-DEFUNI").Scan(&status))
	assert.Equal(t, "RUNNING", status)

	cancelRun()
	require.NoError(t, <-runErr, "clean shutdown returns nil")
}

// TestLiveEmptyUniverseGoesUnhealthy is finding 2: a `multi` node with NO
// --tickers AND an empty stock universe (no SF1 tickers, no bars) cannot
// assemble a session on any restart. The supervisor crash-loops, and after
// liveUnhealthyAfter consecutive failures SessionHealth().Healthy is false — so
// /healthz reports degraded (NOT a misleading green probe over a dead session).
func TestLiveEmptyUniverseGoesUnhealthy(t *testing.T) {
	pool := requirePG(t)
	truncateTickers(t, pool) // empty SF1 universe
	ctx := testCtx(t)

	addr := startMockOpenD(t, ctx, pool)

	node, err := runner.NewLive(pool, nil, runner.LiveConfig{
		TraderID:        "SIGNAL-EMPTY",
		Mode:            "signal",
		Strategy:        "multi", // SEPA-bearing; no tickers + empty universe -> assembly fails
		StartingBalance: 100000,
		MoomooAddr:      addr,
		BarSeconds:      86400,
	}, zerolog.Nop())
	require.NoError(t, err)

	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	runErr := make(chan error, 1)
	go func() { runErr <- node.Run(runCtx) }()

	// The session never runs; after enough consecutive failures the node is
	// reported unhealthy. (The 2s supervisor backoff means ~3 failures take ~6s.)
	require.Eventually(t, func() bool {
		h := node.SessionHealth()
		return !h.Healthy && !h.Running && h.ConsecutiveFailures >= 3
	}, 25*time.Second, 200*time.Millisecond,
		"crash-looping node (empty universe) must report unhealthy on SessionHealth")

	// The failure surfaces the assembly error (no secrets), proving the cause is
	// the empty universe and not, say, a transient feed error.
	h := node.SessionHealth()
	assert.Contains(t, h.LastError, "stock universe",
		"the surfaced failure names the empty-universe assembly error")

	cancelRun()
	<-runErr
}

// TestAssembleDefaultUniverseFromDB is the direct unit of finding 1's root-cause
// fix: Assembler.Assemble with an empty Tickers list for a SEPA-bearing strategy
// resolves the SF1 window universe from the DB.
func TestAssembleDefaultUniverseFromDB(t *testing.T) {
	pool := requirePG(t)
	truncateTickers(t, pool)
	ctx := testCtx(t)

	stocks := []string{"DDD", "EEE"}
	seedSF1Tickers(t, pool, stocks)
	end := time.Now().UTC().Truncate(24 * time.Hour)
	seedDailyBars(t, pool, append(append([]string{}, stocks...), "SPY"), tradingDates(end, 40))

	asm := runner.NewAssembler(pool, "")
	start := calendar.NewDate(end.Year(), end.Month(), end.Day()).AddDays(-30)
	asOf := calendar.NewDate(end.Year(), end.Month(), end.Day())

	as, err := asm.Assemble(ctx, runner.AssemblyInput{
		Strategy:        "multi",
		StartingBalance: 100000,
	}, start, asOf)
	require.NoError(t, err, "multi with empty Tickers must resolve a default universe")
	require.NotNil(t, as)
	// The resolved universe (Tickers) includes the seeded SF1 stocks.
	assert.Subset(t, as.Tickers, stocks,
		"assembled universe should contain the default SF1 stocks")
}

// TestAssembleEmptyUniverseErrorsClearly proves the empty-universe case yields a
// clear actionable error (not the opaque downstream "sepa needs at least one
// stock"), so an operator knows to load bars / pass --tickers.
func TestAssembleEmptyUniverseErrorsClearly(t *testing.T) {
	pool := requirePG(t)
	truncateTickers(t, pool)
	ctx := testCtx(t)

	asm := runner.NewAssembler(pool, "")
	end := time.Now().UTC().Truncate(24 * time.Hour)
	start := calendar.NewDate(end.Year(), end.Month(), end.Day()).AddDays(-30)
	asOf := calendar.NewDate(end.Year(), end.Month(), end.Day())

	_, err := asm.Assemble(ctx, runner.AssemblyInput{
		Strategy:        "sepa",
		StartingBalance: 100000,
	}, start, asOf)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stock universe",
		"empty-universe error should name the missing stock universe")
}
