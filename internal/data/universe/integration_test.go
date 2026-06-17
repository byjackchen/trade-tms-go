//go:build integration

package universe

// End-to-end test against the compose stack (postgres:55432) holding the
// 48-ticker P0 import subset, imported via
//
//	bin/tms import sharadar --tables tickers,sep,sfp --tickers <subset> --since 2024-01-01
//	bin/tms import sharadar --tables sf1,events     --tickers <subset>
//
// It verifies the FULL pipeline — PG window
// universe, fixed-point bar round-trip, market caps, screener ranking,
// cap, snapshot persist + readers — against the same golden fixture as the
// hermetic tests. Run: make itest.

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/config"
	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/db"
)

func itestPool(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("TMS_PG_HOST") == "" || os.Getenv("TMS_PG_PORT") == "" {
		t.Skip("integration: TMS_PG_HOST/TMS_PG_PORT not set (use `make itest`)")
	}
	cfg, err := config.Load()
	require.NoError(t, err)
	pool, err := db.NewPool(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// requireSubsetDB skips unless tms.tickers holds exactly the 48-ticker
// golden subset (extra tickers would legitimately change the universe).
func requireSubsetDB(t *testing.T, ctx context.Context, store *Store, g *goldenFile) {
	t.Helper()
	all, err := store.ListUniverseForWindow(ctx,
		calendar.NewDate(1900, time.January, 1), calendar.NewDate(2100, time.January, 1), TableAny)
	require.NoError(t, err)
	if len(all) == 0 {
		t.Skip("integration: tms.tickers is empty — run the P0 subset import first (see file header)")
	}
	if len(all) != len(g.Subset) {
		t.Skipf("integration: DB holds %d tickers, golden subset needs exactly %d", len(all), len(g.Subset))
	}
	assert.Equal(t, g.Subset, all, "DB ticker set must be the golden 48-ticker subset")
}

// asOfClock returns an instant whose America/New_York date is the golden
// as-of trading date (16:30 ET, after the close).
func asOfClock(t *testing.T, cal *calendar.Calendar, g *goldenFile) time.Time {
	t.Helper()
	d, err := calendar.ParseDate(g.AsOf)
	require.NoError(t, err)
	return time.Date(d.Year, d.Month, d.Day, 16, 30, 0, 0, cal.Location())
}

func TestIntegrationUniversePipeline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	pool := itestPool(t, ctx)
	store := NewStore(pool)
	g := loadGolden(t)
	requireSubsetDB(t, ctx, store, g)

	cal, err := calendar.NewNYSE()
	require.NoError(t, err)
	builder := NewBuilder(store, cal, zerolog.Nop())
	now := asOfClock(t, cal, g)

	t.Run("window-universe", func(t *testing.T) {
		asOf, _ := calendar.ParseDate(g.AsOf)
		start, _ := calendar.ParseDate(g.WarmupStart)

		sf1, err := store.ListUniverseForWindow(ctx, start, asOf, TableSF1)
		require.NoError(t, err)
		// SF1 window list before exclusions == golden universe plus any
		// excluded SF1 names; with this subset the exclusions are all SFP,
		// so it must match exactly.
		assert.Equal(t, g.Universe, sf1)

		sfp, err := store.ListUniverseForWindow(ctx, start, asOf, TableSFP)
		require.NoError(t, err)
		assert.Len(t, sfp, 12, "SPY + 11 sector ETFs")
		assert.Contains(t, sfp, "SPY")

		active, err := store.ListActiveTickers(ctx, asOf)
		require.NoError(t, err)
		assert.Equal(t, g.Subset, active, "all 48 active on as-of")

		_, err = store.ListUniverseForWindow(ctx, start, asOf, "BOGUS")
		require.Error(t, err)
	})

	t.Run("bars-roundtrip", func(t *testing.T) {
		asOf, _ := calendar.ParseDate(g.AsOf)
		start, _ := calendar.ParseDate(g.WarmupStart)

		rows, err := store.GetBars(ctx, "AAPL", start, asOf)
		require.NoError(t, err)
		want := goldenRows(t, g, "AAPL") // the golden tail(260)
		require.GreaterOrEqual(t, len(rows), len(want))
		got := rows[len(rows)-len(want):]
		for i, w := range want {
			assert.Equal(t, w.TS, got[i].TS, "row %d ts", i)
			assert.Equal(t, w.Open, got[i].Open, "row %d open (fixed-point bridge must be lossless)", i)
			assert.Equal(t, w.High, got[i].High, "row %d high", i)
			assert.Equal(t, w.Low, got[i].Low, "row %d low", i)
			assert.Equal(t, w.Close, got[i].Close, "row %d close", i)
			assert.Equal(t, w.Volume, got[i].Volume, "row %d volume", i)
		}

		none, err := store.GetBars(ctx, "NO-SUCH-TICKER", start, asOf)
		require.NoError(t, err, "unknown ticker: empty, never an error")
		assert.Empty(t, none)
	})

	t.Run("market-caps", func(t *testing.T) {
		caps, err := store.MarketCaps(ctx, g.Subset)
		require.NoError(t, err)
		for _, tk := range g.Subset {
			assert.Equal(t, g.MarketCaps[tk], caps[tk], "market cap %s", tk)
		}

		cc := store.NewCapCache(ctx)
		assert.Equal(t, g.MarketCaps["AAPL"], cc.Lookup("AAPL"))
		assert.Equal(t, 0.0, cc.Lookup("SPY"), "no SF1 -> 0.0")
		assert.Equal(t, 0.0, cc.Lookup("NO-SUCH"), "unknown -> 0.0")
		require.NoError(t, cc.Err())
	})

	t.Run("build-limit85-golden-ranking", func(t *testing.T) {
		res, err := builder.Build(ctx, BuildParams{Now: now, Limit: 85, Kind: KindEOD})
		require.NoError(t, err)
		assert.Equal(t, g.AsOf, res.AsOf.String())
		assert.Equal(t, g.WarmupStart, res.WarmupStart.String())
		assert.Equal(t, g.Universe, res.Raw)
		assert.Equal(t, g.Capped85, res.Tickers, "36 <= 85: pass-through")
		assert.Empty(t, res.WarmupErrors)
		assert.Equal(t, len(g.Universe), res.Warmed)

		require.Len(t, res.Candidates, len(g.TopK))
		for i, want := range g.TopK {
			c := res.Candidates[i]
			assert.Equal(t, want.InstrumentID, c.InstrumentID, "rank %d", i+1)
			assert.Equal(t, want.Score, c.Score, "rank %d (%s) score bit-identical through PG", i+1, want.InstrumentID)
			assert.Equal(t, want.TrendTemplateCount, c.Metadata["trend_template_count"], "%s tt", want.InstrumentID)
			assert.Equal(t, want.BreakoutProximity, c.Metadata["breakout_proximity"], "%s prox", want.InstrumentID)
			assert.Equal(t, want.MarketCapUSD, c.Metadata["market_cap_usd"], "%s cap", want.InstrumentID)
		}
		for ticker, want := range g.TrendTemplate {
			got, ok := res.Rules[ticker]
			require.True(t, ok, ticker)
			assert.Equal(t, want.Rules, got.Rules, "%s rule flags", ticker)
			assert.Equal(t, want.MA200UptrendDays, got.MA200UptrendDays, "%s uptrend days", ticker)
		}
	})

	t.Run("build-limit10-cap", func(t *testing.T) {
		res, err := builder.Build(ctx, BuildParams{Now: now, Limit: 10, Kind: KindManual})
		require.NoError(t, err)
		assert.Equal(t, g.Capped10, res.Tickers, "top-10 by market cap descending")
		assert.Len(t, res.Candidates, 10)
	})

	t.Run("build-uncapped-backtest", func(t *testing.T) {
		res, err := builder.Build(ctx, BuildParams{Now: now, Uncapped: true, Kind: KindBacktest, TopK: 5})
		require.NoError(t, err)
		assert.Equal(t, g.Universe, res.Tickers, "backtest path: no cap")
		assert.Len(t, res.Candidates, 5)
		assert.Equal(t, g.TopK[0].InstrumentID, res.Candidates[0].InstrumentID)
	})

	t.Run("snapshot-persist-and-read", func(t *testing.T) {
		res, err := builder.Build(ctx, BuildParams{Now: now, Limit: 85, Kind: KindManual})
		require.NoError(t, err)

		snap := SnapshotFromResult(res)
		require.NoError(t, store.InsertSnapshot(ctx, snap))
		require.NotZero(t, snap.ID)
		require.False(t, snap.CreatedAt.IsZero())

		byID, err := store.SnapshotByID(ctx, snap.ID)
		require.NoError(t, err)
		assert.Equal(t, snap.AsOf, byID.AsOf)
		assert.Equal(t, KindManual, byID.Kind)
		assert.Equal(t, TableSF1, byID.TableFilter)
		assert.Equal(t, snap.WindowStart, byID.WindowStart)
		assert.Equal(t, snap.WindowEnd, byID.WindowEnd)
		assert.Equal(t, 85, byID.LimitN)
		assert.Equal(t, snap.Tickers, byID.Tickers)
		assert.Equal(t, snap.Members, byID.Members)
		assert.Equal(t, []string{}, byID.Excluded)

		latest, err := store.LatestSnapshot(ctx, KindManual)
		require.NoError(t, err)
		assert.GreaterOrEqual(t, latest.ID, snap.ID)

		pit, err := store.SnapshotAsOf(ctx, KindManual, snap.AsOf)
		require.NoError(t, err)
		assert.Equal(t, snap.AsOf, pit.AsOf)

		_, err = store.LatestSnapshot(ctx, KindLive)
		if err != nil {
			assert.ErrorIs(t, err, ErrNoSnapshot)
		}
		_, err = store.SnapshotByID(ctx, 1<<60)
		assert.ErrorIs(t, err, ErrNoSnapshot)
	})

	t.Run("context-cancellation", func(t *testing.T) {
		cctx, ccancel := context.WithCancel(ctx)
		ccancel()
		_, err := builder.Build(cctx, BuildParams{Now: now, Limit: 85, Kind: KindManual})
		require.Error(t, err)
	})
}
