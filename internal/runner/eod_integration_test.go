package runner_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/runner"
)

// sectorETFs is the embedded sector_rotation baseline universe (the 11 SPDR
// sector ETFs). EOD/live with strategy=sector_rotation needs only these bars
// (no SEPA warmup, no SPY context), making it the lightest-dependency strategy
// to drive an integration test.
var sectorETFs = []string{"XLK", "XLF", "XLE", "XLV", "XLY", "XLP", "XLU", "XLB", "XLI", "XLRE", "XLC"}

// seedDailyBars inserts a rising daily bar series for each symbol across the
// given trading dates (UTC midnight), so a sector momentum lookback produces a
// real rebalance and evaluate_intent emits non-trivial intents.
func seedDailyBars(t *testing.T, pool *pgxpool.Pool, symbols []string, dates []time.Time) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, sym := range symbols {
		for i, d := range dates {
			// Rising close: 100 + i, in 1e-4 fixed point. Flat OHLC for simplicity.
			px := int64((100 + i) * 10000)
			vol := int64(1_000_000)
			_, err := pool.Exec(ctx,
				`INSERT INTO tms.bars_daily (ticker, ts, source, open, high, low, close, volume)
				 VALUES ($1, $2, 'SFP', $3, $3, $3, $3, $4)
				 ON CONFLICT (ticker, ts, source) DO UPDATE SET close = EXCLUDED.close`,
				sym, d, px, vol)
			require.NoError(t, err)
		}
	}
}

// tradingDates returns n consecutive UTC-midnight dates ending at end.
func tradingDates(end time.Time, n int) []time.Time {
	out := make([]time.Time, 0, n)
	for i := n - 1; i >= 0; i-- {
		out = append(out, end.AddDate(0, 0, -i))
	}
	return out
}

func countIntentRows(t *testing.T, pool *pgxpool.Pool, asOf string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var n int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM tms.signal_intents WHERE as_of = $1`, asOf).Scan(&n))
	return n
}

// TestEODIdempotency is the core decision-4 contract: running the EOD refresh
// TWICE for the same as_of yields the SAME signal_intents rows — no duplicates.
func TestEODIdempotency(t *testing.T) {
	pool := requirePG(t)
	ctx := testCtx(t)

	asOfDate := time.Date(2024, time.March, 15, 0, 0, 0, 0, time.UTC)
	// Seed 40 daily bars up to as_of so the 12-month-ish momentum lookback has
	// data (sector baseline lookback is well under 40 bars on a daily grid; the
	// rising series guarantees a rebalance + emitted intents).
	dates := tradingDates(asOfDate, 40)
	seedDailyBars(t, pool, sectorETFs, dates)

	asOf := calendar.NewDate(2024, time.March, 15)
	eod := runner.NewEOD(pool, "", zerolog.Nop())
	cfg := runner.EODConfig{
		AsOf:               asOf,
		Strategy:           "sector_rotation",
		StartingBalance:    100000,
		WindowCalendarDays: 60, // covers the 40 seeded bars
	}

	rep1, err := eod.RunRefresh(ctx, cfg, nil)
	require.NoError(t, err)
	require.Positive(t, rep1.IntentRows, "first refresh should upsert intent rows")
	require.Positive(t, rep1.BarsReplayed, "first refresh should replay bars")

	// IntentRows is the count of UPSERT operations (one per ETF per replayed
	// timestamp); the DB keeps only the LATEST as_of row per (strategy_id,
	// symbol, as_of), so the distinct row count is one per ETF — the upsert
	// collapses the whole replay window onto the as_of key.
	rowsAfter1 := countIntentRows(t, pool, "2024-03-15")
	assert.Equal(t, len(sectorETFs), rowsAfter1, "one collapsed row per ETF after first refresh")
	assert.Greater(t, rep1.IntentRows, rowsAfter1, "more upsert ops than rows (window collapses onto as_of)")

	// Re-run: the SECOND run must OVERWRITE, not append.
	rep2, err := eod.RunRefresh(ctx, cfg, nil)
	require.NoError(t, err)

	rowsAfter2 := countIntentRows(t, pool, "2024-03-15")
	assert.Equal(t, rowsAfter1, rowsAfter2, "IDEMPOTENT: re-run must NOT add rows (upsert overwrites)")
	assert.Equal(t, rep1.IntentRows, rep2.IntentRows, "same number of upserts both runs")

	// Every (strategy_id, symbol, as_of) is unique (the partial-unique index).
	var distinct int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(DISTINCT (strategy_id, symbol, as_of)) FROM tms.signal_intents WHERE as_of='2024-03-15'`).Scan(&distinct))
	assert.Equal(t, rowsAfter2, distinct, "no duplicate (strategy_id, symbol, as_of) tuples")

	// One row per ETF in the universe (sector emits one intent per ETF per ts;
	// the upsert keeps only the latest as_of row).
	assert.Equal(t, len(sectorETFs), rowsAfter2, "one upserted row per ETF")
}

// TestEODDeterministicContent proves the upserted intent JSON is byte-identical
// across re-runs (idempotency is content-stable, not just count-stable).
func TestEODDeterministicContent(t *testing.T) {
	pool := requirePG(t)
	ctx := testCtx(t)

	asOfDate := time.Date(2024, time.April, 10, 0, 0, 0, 0, time.UTC)
	seedDailyBars(t, pool, sectorETFs, tradingDates(asOfDate, 40))

	asOf := calendar.NewDate(2024, time.April, 10)
	eod := runner.NewEOD(pool, "", zerolog.Nop())
	cfg := runner.EODConfig{AsOf: asOf, Strategy: "sector_rotation", StartingBalance: 100000, WindowCalendarDays: 60}

	_, err := eod.RunRefresh(ctx, cfg, nil)
	require.NoError(t, err)
	first := snapshotIntents(t, pool, "2024-04-10")

	_, err = eod.RunRefresh(ctx, cfg, nil)
	require.NoError(t, err)
	second := snapshotIntents(t, pool, "2024-04-10")

	assert.Equal(t, first, second, "re-run must produce byte-identical intent content")
}

// snapshotIntents returns a stable map symbol -> intent JSON for an as_of.
func snapshotIntents(t *testing.T, pool *pgxpool.Pool, asOf string) map[string]string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := pool.Query(ctx,
		`SELECT symbol, state, strength, generation, intent::text
		   FROM tms.signal_intents WHERE as_of=$1 ORDER BY symbol`, asOf)
	require.NoError(t, err)
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var sym, state, intent string
		var strength float64
		var gen int64
		require.NoError(t, rows.Scan(&sym, &state, &strength, &gen, &intent))
		out[sym] = state + "|" + intent
	}
	require.NoError(t, rows.Err())
	return out
}
