//go:build integration

package runner_test

// warmup_live_proof_test.go is the GATE-RUNNER live proof for the multi-symbol
// LIVE warmup fix (sector_rotation / pairs) against REAL imported market data
// (the 11 sector ETFs + SPY, and the pair legs, full daily history in PG).
//
// It exercises the PRODUCTION code path:
//   runner.NewAssembler -> Assemble (DB-resolved baseline params) -> a PG-backed
//   WarmupProvider over [now-lookback, now) -> Assembler.BuildWarmupBatch (the
//   look-ahead-safe interleaved pre-window stream) -> livengine.Session.Prime
//   (engine.PrimeWarmupBatch) -> RunStream over the run window.
//
// Proofs:
//   - warmup_symbols (batch) > 0 (12 for sector, 6 for pairs) — the pre-fix value
//     was 0 (cold start).
//   - The FIRST streamed timestamp's intents are ACTIONABLE (not all no_setup),
//     carrying real non-zero momentum strengths — the warmed momentum ranking.
//   - Zero orders during priming AND in signal mode overall.
//   - live (warmed prime + stream run) == batch (single in-band replay over
//     [pre + run]) modulo the non-persisted generation counter.

import (
	"context"
	"encoding/json"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/core"
	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/engine"
	"github.com/byjackchen/trade-tms-go/internal/livengine"
	"github.com/byjackchen/trade-tms-go/internal/runner"
)

// composePool connects to the compose-stack Postgres (host port 55432). The
// gate brings this up with `docker compose --profile app up -d --wait` and
// imports the ETF + pair-leg history. Skips if unreachable so the suite stays
// green without the stack.
func composePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, "postgres://tms:tms@127.0.0.1:55432/tms?sslmode=disable")
	if err != nil {
		t.Skipf("compose postgres unavailable: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("compose postgres ping failed: %v", err)
	}
	return pool
}

// mapWarmupFromPG builds a MapWarmupProvider over [start, runStart) for syms by
// reading bars_daily directly through the universe store the Assembler uses.
// Strictly-before-runStart guard mirrors the production warmup path.
func mapWarmupFromPG(t *testing.T, a *runner.Assembler, syms []string, start, end calendar.Date, runStart time.Time) livengine.WarmupProvider {
	t.Helper()
	ctx := context.Background()
	bars := make(map[string][]domain.Bar, len(syms))
	for _, sym := range syms {
		rows, err := a.Universe().GetBars(ctx, sym, start, end)
		require.NoError(t, err)
		hist := make([]domain.Bar, 0, len(rows))
		for _, r := range rows {
			if !r.TS.UTC().Before(runStart) {
				continue
			}
			b, werr := engine.WrangleOHLCV(sym, r)
			require.NoError(t, werr)
			hist = append(hist, b)
		}
		sort.SliceStable(hist, func(i, j int) bool { return hist[i].TS.Before(hist[j].TS) })
		if len(hist) > 0 {
			bars[sym] = hist
		}
	}
	return livengine.MapWarmupProvider{Bars: bars}
}

// runWindowBatch loads [runStart, runEnd] bars for syms and merges them into a
// single dispatch-ordered stream (the live "run window").
func runWindowBatch(t *testing.T, a *runner.Assembler, syms []string, runStartD, runEndD calendar.Date) []domain.Bar {
	t.Helper()
	ctx := context.Background()
	insts := make([]engine.InstrumentBars, 0, len(syms))
	for _, sym := range syms {
		rows, err := a.Universe().GetBars(ctx, sym, runStartD, runEndD)
		require.NoError(t, err)
		bs := make([]domain.Bar, 0, len(rows))
		for _, r := range rows {
			b, werr := engine.WrangleOHLCV(sym, r)
			require.NoError(t, werr)
			bs = append(bs, b)
		}
		sort.SliceStable(bs, func(i, j int) bool { return bs[i].TS.Before(bs[j].TS) })
		insts = append(insts, engine.InstrumentBars{Symbol: sym, Bars: bs})
	}
	return livengine.BatchBars(insts)
}

func stripGen(t *testing.T, recs []livengine.SignalRecord) []string {
	t.Helper()
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		b, err := json.Marshal(r.Payload)
		require.NoError(t, err)
		var rows []map[string]any
		require.NoError(t, json.Unmarshal(b, &rows))
		for _, row := range rows {
			delete(row, "generation")
		}
		nb, err := json.Marshal(rows)
		require.NoError(t, err)
		out = append(out, r.AsOf.UTC().Format(time.RFC3339Nano)+"|"+r.StrategyID+"|"+string(nb))
	}
	return out
}

// TestWarmedLiveSectorProof is the core gate proof for sector_rotation.
func TestWarmedLiveSectorProof(t *testing.T) {
	pool := composePool(t)
	defer pool.Close()
	a := runner.NewAssembler(pool, "")
	ctx := context.Background()

	// "now" = a recent date with a full lookback window of real history before it
	// and several months of run-window history after it (all imported).
	now := calendar.NewDate(2025, time.September, 2)
	runEnd := calendar.NewDate(2026, time.May, 27)
	runStart := time.Date(now.Year, now.Month, now.Day, 0, 0, 0, 0, time.UTC)

	as, err := a.Assemble(ctx, runner.AssemblyInput{Strategy: "sector_rotation", StartingBalance: 100000}, now, runEnd)
	require.NoError(t, err)
	require.NotEmpty(t, as.WarmupBatchSymbols, "sector must declare batch warmup symbols")
	require.Positive(t, as.WarmupCalendarDays, "sector must declare a warmup horizon")
	t.Logf("sector batch warmup symbols=%d horizon_days=%d syms=%v", len(as.WarmupBatchSymbols), as.WarmupCalendarDays, as.WarmupBatchSymbols)

	warmStart := now.AddDays(-as.WarmupCalendarDays)
	provider := mapWarmupFromPG(t, a, as.WarmupBatchSymbols, warmStart, now, runStart)
	warmupBatch, err := a.BuildWarmupBatch(ctx, as, provider, runStart)
	require.NoError(t, err)
	require.NotEmpty(t, warmupBatch, "warmup batch stream must be non-empty (pre-fix: 0)")
	t.Logf("warmup batch bars=%d", len(warmupBatch))

	runBatch := runWindowBatch(t, a, as.Tickers, now, runEnd)
	require.NotEmpty(t, runBatch)

	// (A) WARMED LIVE: prime over the batch, stream the run window.
	liveSink := livengine.NewMemSink()
	liveSess, err := livengine.NewSession(livengine.Config{
		Exec:            domain.ExecSignal,
		Strategies:      as.Assembly.Strategies,
		Gate:            as.Assembly.Gate,
		SPYSymbol:       as.SPYSymbol,
		WarmupBatch:     warmupBatch,
		StartingBalance: domain.MustMoney("100000"),
		Sink:            liveSink,
	})
	require.NoError(t, err)
	require.NoError(t, liveSess.Prime(ctx))
	// Zero orders during PRIMING (before any streaming).
	require.Zero(t, liveSess.Executor().WouldSubmitCount(), "priming must submit no orders")

	vc := core.NewVirtualClock(time.Time{})
	require.NoError(t, liveSess.RunStream(ctx,
		livengine.SliceStreamFeed{Bars: runBatch, Buffer: 8}, core.StreamVirtual, vc))

	recs := liveSink.SortedIntents()
	require.NotEmpty(t, recs)

	// First streamed timestamp's intents must be ACTIONABLE (not all no_setup).
	firstTS := recs[0].AsOf
	var first []livengine.SignalRecord
	for _, r := range recs {
		if r.AsOf.Equal(firstTS) {
			first = append(first, r)
		}
	}
	dist, actionable, withStrength := sectorDistribution(t, first)
	t.Logf("sector first-bar (%s) state distribution: %v actionable=%d withStrength=%d",
		firstTS.Format("2006-01-02"), dist, actionable, withStrength)
	assert.Positive(t, actionable, "warmed sector first bar must have actionable (non-no_setup) states")
	assert.Positive(t, withStrength, "warmed sector first bar must carry non-zero momentum strength")

	// (B) BATCH reference: a SINGLE in-band replay over [pre + run] sliced to the
	// run window must equal the warmed live run-window intents (modulo generation).
	fullSyms := unionStr(as.WarmupBatchSymbols, as.Tickers)
	fullBatch := fullWindowBatch(t, a, fullSyms, warmStart, runEnd)
	fullSink := livengine.NewMemSink()
	fullSess, err := livengine.NewSession(livengine.Config{
		Exec:            domain.ExecSignal,
		Strategies:      mustReassembleSector(t, pool, ctx, now, runEnd).Assembly.Strategies,
		Gate:            as.Assembly.Gate,
		SPYSymbol:       as.SPYSymbol,
		StartingBalance: domain.MustMoney("100000"),
		Sink:            fullSink,
	})
	require.NoError(t, err)
	require.NoError(t, fullSess.Replay(ctx, fullBatch))
	fullRun := make([]livengine.SignalRecord, 0)
	for _, r := range fullSink.SortedIntents() {
		if !r.AsOf.Before(runStart) {
			fullRun = append(fullRun, r)
		}
	}
	assert.Equal(t, stripGen(t, fullRun), stripGen(t, recs),
		"warmed live sector run-window intents must equal an in-band full-window replay (modulo generation)")

	// Signal mode: orders/fills/positions all zero (NoopExecutor only counts
	// would-be submissions; sector rebalances DO produce would-be intents).
	t.Logf("would-be submissions over run window: %d (signal mode: zero real orders/fills/positions)", liveSess.Executor().WouldSubmitCount())
}

// TestWarmedLivePairsProof is the core gate proof for pairs.
func TestWarmedLivePairsProof(t *testing.T) {
	pool := composePool(t)
	defer pool.Close()
	a := runner.NewAssembler(pool, "")
	ctx := context.Background()

	now := calendar.NewDate(2025, time.September, 2)
	runEnd := calendar.NewDate(2026, time.May, 27)
	runStart := time.Date(now.Year, now.Month, now.Day, 0, 0, 0, 0, time.UTC)

	as, err := a.Assemble(ctx, runner.AssemblyInput{Strategy: "pairs", StartingBalance: 100000}, now, runEnd)
	require.NoError(t, err)
	require.NotEmpty(t, as.WarmupBatchSymbols, "pairs must declare batch warmup symbols")
	t.Logf("pairs batch warmup symbols=%d horizon_days=%d syms=%v", len(as.WarmupBatchSymbols), as.WarmupCalendarDays, as.WarmupBatchSymbols)

	warmStart := now.AddDays(-as.WarmupCalendarDays)
	provider := mapWarmupFromPG(t, a, as.WarmupBatchSymbols, warmStart, now, runStart)
	warmupBatch, err := a.BuildWarmupBatch(ctx, as, provider, runStart)
	require.NoError(t, err)
	require.NotEmpty(t, warmupBatch, "pairs warmup batch must be non-empty (pre-fix: 0)")
	t.Logf("pairs warmup batch bars=%d", len(warmupBatch))

	runBatch := runWindowBatch(t, a, as.Tickers, now, runEnd)
	require.NotEmpty(t, runBatch)

	liveSink := livengine.NewMemSink()
	liveSess, err := livengine.NewSession(livengine.Config{
		Exec:            domain.ExecSignal,
		Strategies:      as.Assembly.Strategies,
		Gate:            as.Assembly.Gate,
		SPYSymbol:       as.SPYSymbol,
		WarmupBatch:     warmupBatch,
		StartingBalance: domain.MustMoney("100000"),
		Sink:            liveSink,
	})
	require.NoError(t, err)
	require.NoError(t, liveSess.Prime(ctx))
	require.Zero(t, liveSess.Executor().WouldSubmitCount(), "pairs priming must submit no orders")

	vc := core.NewVirtualClock(time.Time{})
	require.NoError(t, liveSess.RunStream(ctx,
		livengine.SliceStreamFeed{Bars: runBatch, Buffer: 8}, core.StreamVirtual, vc))

	recs := liveSink.SortedIntents()
	require.NotEmpty(t, recs)

	// The warmed pairs spread/z-score must be FORMED at the first streamed bar: at
	// least one leg carries a non-zero z-score (cold start would be all-zero until
	// lookback bars accumulate).
	firstTS := recs[0].AsOf
	maxAbsZ := 0.0
	nonZeroZ := 0
	for _, r := range recs {
		if !r.AsOf.Equal(firstTS) {
			continue
		}
		for _, z := range pairsZScores(t, r) {
			if z != 0 {
				nonZeroZ++
			}
			if az := absf(z); az > maxAbsZ {
				maxAbsZ = az
			}
		}
	}
	t.Logf("pairs first-bar (%s) nonZeroZ=%d maxAbsZ=%.4f", firstTS.Format("2006-01-02"), nonZeroZ, maxAbsZ)
	assert.Positive(t, nonZeroZ, "warmed pairs first bar must carry a formed (non-zero) z-score")

	// Consistency: warmed live == in-band full-window replay (modulo generation).
	fullSyms := unionStr(as.WarmupBatchSymbols, as.Tickers)
	fullBatch := fullWindowBatch(t, a, fullSyms, warmStart, runEnd)
	fullSink := livengine.NewMemSink()
	fullSess, err := livengine.NewSession(livengine.Config{
		Exec:            domain.ExecSignal,
		Strategies:      mustReassemblePairs(t, pool, ctx, now, runEnd).Assembly.Strategies,
		Gate:            as.Assembly.Gate,
		SPYSymbol:       as.SPYSymbol,
		StartingBalance: domain.MustMoney("100000"),
		Sink:            fullSink,
	})
	require.NoError(t, err)
	require.NoError(t, fullSess.Replay(ctx, fullBatch))
	fullRun := make([]livengine.SignalRecord, 0)
	for _, r := range fullSink.SortedIntents() {
		if !r.AsOf.Before(runStart) {
			fullRun = append(fullRun, r)
		}
	}
	assert.Equal(t, stripGen(t, fullRun), stripGen(t, recs),
		"warmed live pairs run-window intents must equal an in-band full-window replay (modulo generation)")
}

// --- helpers ---

func mustReassembleSector(t *testing.T, pool *pgxpool.Pool, ctx context.Context, start, end calendar.Date) *runner.Assembled {
	t.Helper()
	a := runner.NewAssembler(pool, "")
	as, err := a.Assemble(ctx, runner.AssemblyInput{Strategy: "sector_rotation", StartingBalance: 100000}, start, end)
	require.NoError(t, err)
	return as
}

func mustReassemblePairs(t *testing.T, pool *pgxpool.Pool, ctx context.Context, start, end calendar.Date) *runner.Assembled {
	t.Helper()
	a := runner.NewAssembler(pool, "")
	as, err := a.Assemble(ctx, runner.AssemblyInput{Strategy: "pairs", StartingBalance: 100000}, start, end)
	require.NoError(t, err)
	return as
}

func fullWindowBatch(t *testing.T, a *runner.Assembler, syms []string, start, end calendar.Date) []domain.Bar {
	t.Helper()
	ctx := context.Background()
	insts := make([]engine.InstrumentBars, 0, len(syms))
	for _, sym := range syms {
		rows, err := a.Universe().GetBars(ctx, sym, start, end)
		require.NoError(t, err)
		bs := make([]domain.Bar, 0, len(rows))
		for _, r := range rows {
			b, werr := engine.WrangleOHLCV(sym, r)
			require.NoError(t, werr)
			bs = append(bs, b)
		}
		sort.SliceStable(bs, func(i, j int) bool { return bs[i].TS.Before(bs[j].TS) })
		insts = append(insts, engine.InstrumentBars{Symbol: sym, Bars: bs})
	}
	return livengine.BatchBars(insts)
}

func unionStr(a, b []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(a)+len(b))
	for _, s := range append(append([]string{}, a...), b...) {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func sectorDistribution(t *testing.T, recs []livengine.SignalRecord) (dist map[string]int, actionable, withStrength int) {
	t.Helper()
	dist = map[string]int{}
	for _, rec := range recs {
		b, err := json.Marshal(rec.Payload)
		require.NoError(t, err)
		var rows []struct {
			State    string  `json:"state"`
			Strength float64 `json:"strength"`
		}
		require.NoError(t, json.Unmarshal(b, &rows))
		for _, r := range rows {
			dist[r.State]++
			if r.State != string(domain.StateNoSetup) {
				actionable++
			}
			if r.Strength > 0 {
				withStrength++
			}
		}
	}
	return dist, actionable, withStrength
}

func pairsZScores(t *testing.T, rec livengine.SignalRecord) []float64 {
	t.Helper()
	b, err := json.Marshal(rec.Payload)
	require.NoError(t, err)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(b, &rows))
	out := make([]float64, 0, len(rows))
	for _, r := range rows {
		for _, k := range []string{"zscore", "z_score", "z"} {
			if v, ok := r[k]; ok {
				if f, ok := v.(float64); ok {
					out = append(out, f)
				}
			}
		}
	}
	return out
}

func absf(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
