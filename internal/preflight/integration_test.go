//go:build integration

package preflight_test

// integration_test.go drives the REAL PG-backed probes against an ephemeral
// TimescaleDB (the shared pgtest harness) so the preflight is verified end-to-end
// over actual SQL: it must FAIL on stale data / missing warmup / missing caps and
// PASS once the tables are seeded correctly. The OpenD + Redis blockers are not
// exercised here (no broker / Redis in the harness); the test runs in signal mode
// so those are advisory, isolating the data-tier checks.

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/preflight"
	"github.com/byjackchen/trade-tms-go/internal/testutil/pgtest"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m, "preflight")) }

func requirePG(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := pgtest.RequirePG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := pool.Exec(ctx, `TRUNCATE tms.tickers, tms.bars_daily, tms.fundamentals_sf1 RESTART IDENTITY CASCADE`)
	require.NoError(t, err)
	return pool
}

// fixedNow is a Wednesday well inside the calendar range; T-1 is Tue 2026-06-09.
var fixedNow = time.Date(2026, time.June, 10, 14, 0, 0, 0, time.UTC)

// seedTicker inserts an SF1 common stock tradable over the whole window.
func seedTicker(t *testing.T, pool *pgxpool.Pool, ticker string) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO tms.tickers (ticker, table_name, first_price_date, last_price_date)
		 VALUES ($1, 'SF1', '2000-01-01', NULL)`, ticker)
	require.NoError(t, err)
}

// seedBars inserts n consecutive daily SEP bars ending at `end` for ticker.
func seedBars(t *testing.T, pool *pgxpool.Pool, ticker string, end calendar.Date, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		d := end.AddDays(-i)
		ts := time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, time.UTC)
		_, err := pool.Exec(ctx,
			`INSERT INTO tms.bars_daily (ticker, ts, source, open, high, low, close, volume)
			 VALUES ($1, $2, 'SEP', 1000000, 1010000, 990000, 1005000, 1000000)
			 ON CONFLICT DO NOTHING`, ticker, ts)
		require.NoError(t, err)
	}
}

// seedCap inserts an SF1 MRT market cap row for ticker.
func seedCap(t *testing.T, pool *pgxpool.Pool, ticker string, cap float64, datekey calendar.Date) {
	t.Helper()
	dk := time.Date(datekey.Year, datekey.Month, datekey.Day, 0, 0, 0, 0, time.UTC)
	_, err := pool.Exec(context.Background(),
		`INSERT INTO tms.fundamentals_sf1 (ticker, datekey, dimension, marketcap)
		 VALUES ($1, $2, 'MRT', $3)`, ticker, dk, cap)
	require.NoError(t, err)
}

func newProbes(t *testing.T, pool *pgxpool.Pool) *preflight.PGProbes {
	t.Helper()
	cal, err := calendar.NewNYSE()
	require.NoError(t, err)
	// A non-nil redis client that will fail to connect — fine, signal mode does
	// not block on Redis here (we assert only the data-tier checks).
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	t.Cleanup(func() { _ = rdb.Close() })
	return preflight.NewPGProbes(preflight.PGProbesConfig{
		Pool:     pool,
		Calendar: cal,
		Redis:    rdb,
		Log:      zerolog.Nop(),
	})
}

func signalCfg(tickers []string) preflight.Config {
	return preflight.Config{
		Mode:                "signal",
		Strategy:            "sepa",
		Tickers:             tickers,
		MaxStaleTradingDays: 1,
		Now:                 func() time.Time { return fixedNow },
	}
}

// findCheck locates a check result by id.
func findCheck(t *testing.T, r preflight.Report, id string) preflight.CheckResult {
	t.Helper()
	for _, c := range r.Checks {
		if c.Check == id {
			return c
		}
	}
	t.Fatalf("check %s not in report", id)
	return preflight.CheckResult{}
}

func TestIntegration_Preflight(t *testing.T) {
	pool := requirePG(t)
	probes := newProbes(t, pool)
	ctx := context.Background()
	tMinus1 := calendar.NewDate(2026, time.June, 9)
	universe := []string{"AAPL", "MSFT"}

	t.Run("stale data fails DATA_CURRENT", func(t *testing.T) {
		_, err := pool.Exec(ctx, `TRUNCATE tms.tickers, tms.bars_daily, tms.fundamentals_sf1 RESTART IDENTITY CASCADE`)
		require.NoError(t, err)
		for _, tk := range universe {
			seedTicker(t, pool, tk)
			// Frontier is two weeks behind T-1 -> stale beyond tolerance.
			seedBars(t, pool, tk, calendar.NewDate(2026, time.May, 26), 300)
			seedCap(t, pool, tk, 1e12, calendar.NewDate(2026, time.March, 31))
		}
		// signal mode: DATA_CURRENT is advisory (warn), not a blocker. The status
		// must still surface staleness as WARN.
		rep := preflight.Run(ctx, signalCfg(universe), probes)
		dc := findCheck(t, rep, preflight.CheckDataCurrent)
		require.Equal(t, preflight.StatusWarn, dc.Status, "stale data must warn: %s", dc.Detail)

		// In PAPER mode the same staleness is a BLOCKER.
		paper := signalCfg(universe)
		paper.Mode = "paper"
		prep := preflight.Run(ctx, paper, probes)
		pdc := findCheck(t, prep, preflight.CheckDataCurrent)
		require.Equal(t, preflight.StatusFail, pdc.Status)
		require.Equal(t, preflight.SeverityBlocker, pdc.Severity)
		require.False(t, prep.OK, "paper report must not be OK with stale data")
	})

	t.Run("missing warmup fails WARMUP_AVAILABLE", func(t *testing.T) {
		_, err := pool.Exec(ctx, `TRUNCATE tms.tickers, tms.bars_daily, tms.fundamentals_sf1 RESTART IDENTITY CASCADE`)
		require.NoError(t, err)
		for _, tk := range universe {
			seedTicker(t, pool, tk)
			// Fresh frontier but only 10 bars — far short of SEPA's 200 lookback.
			seedBars(t, pool, tk, tMinus1, 10)
			seedCap(t, pool, tk, 1e12, calendar.NewDate(2026, time.March, 31))
		}
		rep := preflight.Run(ctx, signalCfg(universe), probes)
		wc := findCheck(t, rep, preflight.CheckWarmupAvailable)
		require.Equal(t, preflight.StatusFail, wc.Status, "thin warmup must fail: %s", wc.Detail)
		require.False(t, rep.OK, "missing warmup is a blocker -> report not OK")
	})

	t.Run("missing caps fails MARKET_DATA_FUNDAMENTALS", func(t *testing.T) {
		_, err := pool.Exec(ctx, `TRUNCATE tms.tickers, tms.bars_daily, tms.fundamentals_sf1 RESTART IDENTITY CASCADE`)
		require.NoError(t, err)
		for _, tk := range universe {
			seedTicker(t, pool, tk)
			seedBars(t, pool, tk, tMinus1, 300)
			// No SF1 caps -> the all-degenerate case.
		}
		rep := preflight.Run(ctx, signalCfg(universe), probes)
		mc := findCheck(t, rep, preflight.CheckMarketDataFund)
		require.Equal(t, preflight.StatusFail, mc.Status, "absent caps must fail: %s", mc.Detail)
		require.False(t, rep.OK, "missing caps is a blocker -> report not OK")
	})

	t.Run("fully seeded passes all data-tier blockers", func(t *testing.T) {
		_, err := pool.Exec(ctx, `TRUNCATE tms.tickers, tms.bars_daily, tms.fundamentals_sf1 RESTART IDENTITY CASCADE`)
		require.NoError(t, err)
		for _, tk := range universe {
			seedTicker(t, pool, tk)
			seedBars(t, pool, tk, tMinus1, 300) // fresh + deep
			seedCap(t, pool, tk, 1e12, calendar.NewDate(2026, time.March, 31))
		}
		rep := preflight.Run(ctx, signalCfg(universe), probes)
		for _, id := range []string{
			preflight.CheckDataCurrent, preflight.CheckUniverse,
			preflight.CheckMarketDataFund, preflight.CheckWarmupAvailable,
		} {
			c := findCheck(t, rep, id)
			require.NotEqual(t, preflight.StatusFail, c.Status, "%s must not fail when seeded: %s", id, c.Detail)
		}
		// PG is up; the data-tier blockers all pass. Redis is down here, so the
		// overall OK bit may be false ONLY because of REDIS — assert that the data
		// blockers are clean by checking no data-check appears among the blockers.
		for _, b := range rep.Blockers() {
			switch b.Check {
			case preflight.CheckDataCurrent, preflight.CheckUniverse,
				preflight.CheckMarketDataFund, preflight.CheckWarmupAvailable:
				t.Fatalf("data-tier check %s blocked when fully seeded: %s", b.Check, b.Detail)
			}
		}
	})
}
