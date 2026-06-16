package livengine_test

// warmup_batch_test.go is the consistency + actionable-state proof for the LIVE
// multi-symbol warmup fix (sector_rotation / pairs). Before the fix only SEPA was
// warmed from history, so a fresh LIVE sector/pairs session started COLD
// (warmup_symbols=0): the momentum ranking / pairs spread had no window, and every
// symbol emitted state=no_setup / strength=0 until ~lookback live bars accumulated
// (months on daily bars). The fix primes those multi-symbol strategies from the
// INTERLEAVED pre-window history via the engine.BatchWarmupConsumer seam.
//
// The proofs here:
//   - TestLiveSectorWarmupEqualsBatchReplay: a warmed live session (Prime over the
//     pre-window batch, then stream the run window) emits run-window intents
//     IDENTICAL to a backtest that processed [pre-window + run-window] in-band —
//     the look-ahead-safe live==batch consistency the fix must hold.
//   - TestLivePairsWarmupEqualsBatchReplay: same proof for pairs.
//   - TestColdSectorIsAllNoSetup_WarmedIsActionable: the cold (pre-fix) session is
//     all-no_setup; the warmed session emits ACTIONABLE states (buy/hold/exit/
//     forming) with real momentum strength.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/core"
	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/engine"
	"github.com/byjackchen/trade-tms-go/internal/engine/strategyassembly"
	"github.com/byjackchen/trade-tms-go/internal/livengine"
	"github.com/byjackchen/trade-tms-go/internal/params"
)

// ---------------------------------------------------------------------------
// sector warmup scenario
// ---------------------------------------------------------------------------

// sectorWarmupParams: 8 ETFs, lookback 2, topK 3 (a real winners/losers ranking
// so warmed intents carry buy/hold/exit/forming states, not a uniform top-8).
func sectorWarmupParams() params.SectorRotationParams {
	return params.SectorRotationParams{
		Universe:         []string{"E1", "E2", "E3", "E4", "E5", "E6", "E7", "E8"},
		MomentumLookback: 2,
		TopK:             3,
		Timezone:         "America/New_York",
	}
}

// sectorPreRun builds the pre-window (warmup) and run-window per-ETF series.
// Pre-window: Nov/Dec/Jan bars per ETF, with DIVERGENT trajectories so the lookback
// momentum ranking is non-trivial (E1..E3 climb hardest -> top-3) and a January
// rebalance fires (full warmup reached). Run-window: February + March bars, each a
// month rollover that emits a rebalance + intents. The two are returned separately
// so the test can prime over `pre` and stream `run`.
func sectorPreRun() (pre, run []engine.InstrumentBars) {
	syms := []string{"E1", "E2", "E3", "E4", "E5", "E6", "E7", "E8"}
	// Per-symbol monthly price level: higher index climbs slower, so E1..E3 win.
	level := func(sym string, step int) string {
		base := map[string]float64{
			"E1": 100, "E2": 100, "E3": 100, "E4": 100,
			"E5": 100, "E6": 100, "E7": 100, "E8": 100,
		}[sym]
		gain := map[string]float64{
			"E1": 12, "E2": 10, "E3": 8, "E4": 6,
			"E5": 4, "E6": 3, "E7": 2, "E8": 1,
		}[sym]
		return fmt.Sprintf("%.2f", base+gain*float64(step))
	}
	for _, s := range syms {
		pre = append(pre, engine.InstrumentBars{Symbol: s, Bars: []domain.Bar{
			// 3 November + December + January closes => > lookback+1, full warmup by Jan.
			bar(s, 2023, time.November, 1, level(s, 0), 1000),
			bar(s, 2023, time.November, 15, level(s, 1), 1000),
			bar(s, 2023, time.December, 1, level(s, 2), 1000),
			bar(s, 2023, time.December, 15, level(s, 3), 1000),
			bar(s, 2024, time.January, 2, level(s, 4), 1000),
			bar(s, 2024, time.January, 16, level(s, 5), 1000),
		}})
		// Run window: E8 SURGES (a late breakout) so it climbs into the top-3 at the
		// March rebalance, forcing a real FLAT/LONG transition (would-be orders > 0).
		runLevel := func(step int) string {
			if s == "E8" {
				return fmt.Sprintf("%.2f", 100.0+40.0*float64(step-5)) // steep late surge
			}
			return level(s, step)
		}
		run = append(run, engine.InstrumentBars{Symbol: s, Bars: []domain.Bar{
			// February + March rollovers -> rebalances + intents in the run window.
			bar(s, 2024, time.February, 1, runLevel(6), 1000),
			bar(s, 2024, time.February, 15, runLevel(7), 1000),
			bar(s, 2024, time.March, 1, runLevel(8), 1000),
		}})
	}
	return pre, run
}

// runWindowIntents filters a sink's sorted intents to those at/after the run-window
// start (the comparison surface: a warmed live session only emits run-window
// intents, so the backtest reference is sliced to the same window).
func runWindowIntents(recs []livengine.IntentRecord, runStart time.Time) []livengine.IntentRecord {
	out := make([]livengine.IntentRecord, 0, len(recs))
	for _, r := range recs {
		if !r.AsOf.Before(runStart) {
			out = append(out, r)
		}
	}
	return out
}

func buildSectorWarmupSession(t *testing.T, sink livengine.IntentSink, batch []domain.Bar) *livengine.Session {
	t.Helper()
	asm, err := strategyassembly.Assemble(strategyassembly.Input{
		Strategy:        "sector_rotation",
		StartingBalance: 100000,
		Params:          strategyassembly.Params{Sector: sectorWarmupParams()},
	})
	require.NoError(t, err)
	sess, err := livengine.NewSession(livengine.Config{
		Exec:            domain.ExecSignal,
		Strategies:      asm.Strategies,
		Portfolio:       asm.Portfolio,
		StartingBalance: domain.MustMoney("100000"),
		WarmupBatch:     batch,
		Sink:            sink,
	})
	require.NoError(t, err)
	return sess
}

// TestLiveSectorWarmupEqualsBatchReplay is the live==batch consistency proof for
// the sector warmup fix: a warmed live session (Prime over the pre-window batch,
// then stream the run window) emits run-window intents IDENTICAL to a single batch
// replay over [pre + run]. The batch consumer replays the pre-window bars through
// the SAME generator OnBar the run loop uses (discarding signals), so the
// generator state at the run-window start coincides.
func TestLiveSectorWarmupEqualsBatchReplay(t *testing.T) {
	pre, run := sectorPreRun()
	preBatch := livengine.BatchBars(pre)
	runBatch := livengine.BatchBars(run)
	fullBatch := livengine.BatchBars(append(append([]engine.InstrumentBars{}, pre...), run...))
	runStart := ts(2024, time.February, 1)

	// (A) Warmed live: prime over pre-window batch, stream the run window.
	liveSink := livengine.NewMemSink()
	liveSess := buildSectorWarmupSession(t, liveSink, preBatch)
	require.NoError(t, liveSess.Prime(context.Background()))
	vc := core.NewVirtualClock(time.Time{})
	require.NoError(t, liveSess.RunStream(context.Background(),
		livengine.SliceStreamFeed{Bars: runBatch, Buffer: 4}, core.StreamVirtual, vc))

	// (B) Backtest reference: a session that processed the SAME pre-window bars
	// (via batch warmup priming) then REPLAYS the run window. This is the faithful
	// "a backtest that processed [now-lookback, now] then those bars" reference:
	// the pre-window bars build the generator state out-of-band (no run-window
	// EvaluateIntent before the run starts, so the intent generation counter — which
	// increments even on warmup and is explicitly NOT persisted — starts from the
	// SAME point as the live path). Replay vs RunStream is the only difference, and
	// the consistency proof is that they coincide.
	batchSink := livengine.NewMemSink()
	batchSess := buildSectorWarmupSession(t, batchSink, preBatch)
	require.NoError(t, batchSess.Prime(context.Background()))
	require.NoError(t, batchSess.Replay(context.Background(), runBatch))

	liveCanon := canonicalIntents(t, liveSink.SortedIntents())
	batchCanon := canonicalIntents(t, batchSink.SortedIntents())

	require.NotEmpty(t, liveCanon)
	assert.Equal(t, batchCanon, liveCanon,
		"warmed live sector intents must equal the warmed batch-replay intents")

	// And the generator state at the run-window start is exactly what a SINGLE
	// in-band replay over [pre + run] reaches: assert the WARMED run-window intents
	// (state/strength/rank/momentum, i.e. everything but the non-persisted
	// generation counter) equal an unwarmed full-window replay sliced to the run
	// window. This is the look-ahead-safe equivalence: out-of-band priming over the
	// pre-window == in-band processing of the same pre-window bars.
	fullSink := livengine.NewMemSink()
	fullSess := buildSectorWarmupSession(t, fullSink, nil)
	require.NoError(t, fullSess.Replay(context.Background(), fullBatch))
	fullRun := runWindowIntents(fullSink.SortedIntents(), runStart)
	assert.Equal(t,
		stripGeneration(t, fullRun),
		stripGeneration(t, liveSink.SortedIntents()),
		"warmed run-window intents must equal an in-band full-window replay (modulo the non-persisted generation counter)")

	// Priming placed NO orders; the streamed rebalances DID fire (would-be > 0).
	assert.Positive(t, liveSess.Executor().WouldSubmitCount(),
		"warmed sector run window should have produced would-be rebalance orders")
}

// TestColdSectorIsAllNoSetup_WarmedIsActionable proves the BUG and the FIX side by
// side: a COLD sector session (no warmup, streaming only the run window) emits
// all-no_setup / strength-0 intents (the momentum window never forms in time),
// whereas a WARMED session (primed over the pre-window batch) emits ACTIONABLE
// states (buy/hold/exit/forming) with real momentum strength from the first bar.
func TestColdSectorIsAllNoSetup_WarmedIsActionable(t *testing.T) {
	pre, run := sectorPreRun()
	preBatch := livengine.BatchBars(pre)
	runBatch := livengine.BatchBars(run)

	// COLD: no warmup, only the 3 run-window bars per ETF. lookback+1 = 3 closes
	// are reached only at the very last bar, and EvaluateIntent's warmup gate keeps
	// every prior timestamp all-no_setup; the early window is useless.
	coldSink := livengine.NewMemSink()
	coldSess := buildSectorWarmupSession(t, coldSink, nil)
	require.NoError(t, coldSess.Prime(context.Background()))
	require.NoError(t, coldSess.Replay(context.Background(), runBatch))
	require.NotEmpty(t, coldSink.Intents)

	// The FIRST timestamp's intents are all no_setup (cold start).
	firstCold := intentsAt(coldSink.SortedIntents(), ts(2024, time.February, 1))
	require.NotEmpty(t, firstCold)
	for _, it := range firstCold {
		st, strength := sectorStateStrength(t, it)
		assert.Equal(t, string(domain.StateNoSetup), st, "cold first bar: every ETF is no_setup")
		assert.Zero(t, strength, "cold first bar: zero momentum strength")
	}

	// WARMED: prime over the pre-window batch, then stream the run window. The
	// momentum ranking is fully formed at session start, so the FIRST streamed
	// timestamp already carries actionable states + real strengths.
	warmSink := livengine.NewMemSink()
	warmSess := buildSectorWarmupSession(t, warmSink, preBatch)
	require.NoError(t, warmSess.Prime(context.Background()))
	vc := core.NewVirtualClock(time.Time{})
	require.NoError(t, warmSess.RunStream(context.Background(),
		livengine.SliceStreamFeed{Bars: runBatch, Buffer: 4}, core.StreamVirtual, vc))

	firstWarm := intentsAt(warmSink.SortedIntents(), ts(2024, time.February, 1))
	require.NotEmpty(t, firstWarm)
	var actionable, withStrength int
	for _, it := range firstWarm {
		st, strength := sectorStateStrength(t, it)
		if st != string(domain.StateNoSetup) {
			actionable++
		}
		if strength > 0 {
			withStrength++
		}
	}
	assert.Positive(t, actionable,
		"warmed first bar: at least one ETF must be actionable (buy/hold/exit/forming), not all no_setup")
	assert.Positive(t, withStrength,
		"warmed first bar: at least one ETF must carry a real momentum strength")
}

// ---------------------------------------------------------------------------
// pairs warmup scenario
// ---------------------------------------------------------------------------

func pairsWarmupParams() params.PairsParams {
	return params.PairsParams{
		Pairs:             []params.Pair{{LongLeg: "KO", ShortLeg: "PEP"}},
		Lookback:          5,
		EntryZ:            1.0,
		ExitZ:             0.3,
		CapitalPerPairPct: 0.30,
		Timezone:          "America/New_York",
	}
}

// pairsPreRun builds a pre-window (>= lookback closes per leg, so the spread is
// formed) and a run window where the z-score crosses the entry/exit thresholds.
func pairsPreRun() (pre, run []engine.InstrumentBars, runStart time.Time) {
	day := ts(2024, time.January, 2)
	mk := func(sym string, closes []string) engine.InstrumentBars {
		d := day
		bars := make([]domain.Bar, 0, len(closes))
		for _, c := range closes {
			bars = append(bars, bar(sym, d.Year(), d.Month(), d.Day(), c, 1000))
			d = d.AddDate(0, 0, 1)
		}
		return engine.InstrumentBars{Symbol: sym, Bars: bars}
	}
	// 8 pre-window closes (> lookback 5) then 6 run-window closes. KO drifts up
	// relative to PEP in the run window to drive the spread z-score across entry.
	koPre := []string{"50.00", "50.20", "50.10", "50.30", "50.25", "50.40", "50.35", "50.50"}
	pepPre := []string{"60.00", "60.10", "60.05", "60.15", "60.10", "60.20", "60.15", "60.25"}
	koRun := []string{"51.50", "52.50", "53.50", "52.00", "51.00", "50.60"}
	pepRun := []string{"60.30", "60.35", "60.40", "60.45", "60.50", "60.55"}

	pre = []engine.InstrumentBars{mk("KO", koPre), mk("PEP", pepPre)}
	// Run-window dates continue after the 8 pre-window days.
	runDay := day.AddDate(0, 0, 8)
	mkRun := func(sym string, closes []string) engine.InstrumentBars {
		d := runDay
		bars := make([]domain.Bar, 0, len(closes))
		for _, c := range closes {
			bars = append(bars, bar(sym, d.Year(), d.Month(), d.Day(), c, 1000))
			d = d.AddDate(0, 0, 1)
		}
		return engine.InstrumentBars{Symbol: sym, Bars: bars}
	}
	run = []engine.InstrumentBars{mkRun("KO", koRun), mkRun("PEP", pepRun)}
	return pre, run, runDay
}

func buildPairsWarmupSession(t *testing.T, sink livengine.IntentSink, batch []domain.Bar) *livengine.Session {
	t.Helper()
	asm, err := strategyassembly.Assemble(strategyassembly.Input{
		Strategy:        "pairs",
		StartingBalance: 100000,
		Params:          strategyassembly.Params{Pairs: pairsWarmupParams()},
	})
	require.NoError(t, err)
	sess, err := livengine.NewSession(livengine.Config{
		Exec:            domain.ExecSignal,
		Strategies:      asm.Strategies,
		Portfolio:       asm.Portfolio,
		StartingBalance: domain.MustMoney("100000"),
		WarmupBatch:     batch,
		Sink:            sink,
	})
	require.NoError(t, err)
	return sess
}

// TestLivePairsWarmupEqualsBatchReplay is the live==batch consistency proof for the
// pairs warmup fix: a warmed live session (Prime over the pre-window batch, then
// stream the run window) emits run-window intents IDENTICAL to a backtest that
// processed [pre + run] in-band. The batch consumer replays the pre-window bars
// through the SAME generator OnDomainBar the run loop uses, so each leg's close
// ring + the pair state machine coincide at the run-window start.
func TestLivePairsWarmupEqualsBatchReplay(t *testing.T) {
	pre, run, runStart := pairsPreRun()
	preBatch := livengine.BatchBars(pre)
	runBatch := livengine.BatchBars(run)
	fullBatch := livengine.BatchBars(append(append([]engine.InstrumentBars{}, pre...), run...))

	// (A) Warmed live.
	liveSink := livengine.NewMemSink()
	liveSess := buildPairsWarmupSession(t, liveSink, preBatch)
	require.NoError(t, liveSess.Prime(context.Background()))
	vc := core.NewVirtualClock(time.Time{})
	require.NoError(t, liveSess.RunStream(context.Background(),
		livengine.SliceStreamFeed{Bars: runBatch, Buffer: 4}, core.StreamVirtual, vc))

	// (B) Backtest reference: a warmed session that primed over the SAME pre-window
	// bars then REPLAYS the run window (the faithful warmed-batch reference — same
	// generation baseline as the live path; Replay vs RunStream is the only diff).
	batchSink := livengine.NewMemSink()
	batchSess := buildPairsWarmupSession(t, batchSink, preBatch)
	require.NoError(t, batchSess.Prime(context.Background()))
	require.NoError(t, batchSess.Replay(context.Background(), runBatch))

	liveCanon := canonicalIntents(t, liveSink.SortedIntents())
	batchCanon := canonicalIntents(t, batchSink.SortedIntents())
	require.NotEmpty(t, liveCanon)
	assert.Equal(t, batchCanon, liveCanon,
		"warmed live pairs intents must equal the warmed batch-replay intents")

	// And modulo the non-persisted generation counter, the warmed run-window intents
	// equal an in-band full-window replay (out-of-band priming == in-band processing
	// of the same pre-window bars).
	fullSink := livengine.NewMemSink()
	fullSess := buildPairsWarmupSession(t, fullSink, nil)
	require.NoError(t, fullSess.Replay(context.Background(), fullBatch))
	assert.Equal(t,
		stripGeneration(t, runWindowIntents(fullSink.SortedIntents(), runStart)),
		stripGeneration(t, liveSink.SortedIntents()),
		"warmed pairs run-window intents must equal an in-band full-window replay (modulo generation)")
}

// stripGeneration canonicalizes intents into comparable (AsOf, StrategyID, JSON)
// tuples with the per-row `generation` field zeroed out. The generation counter
// increments on EVERY EvaluateIntent call (including out-of-band warmup) and is
// explicitly NOT persisted, so it is the ONE field that legitimately differs
// between an out-of-band-primed run and an in-band full-window replay. Everything
// else (state, strength, rank, momentum_score, weights) must coincide.
func stripGeneration(t *testing.T, recs []livengine.IntentRecord) []string {
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

// intentsAt returns the recorded intents at exactly asOf.
func intentsAt(recs []livengine.IntentRecord, asOf time.Time) []livengine.IntentRecord {
	out := make([]livengine.IntentRecord, 0)
	for _, r := range recs {
		if r.AsOf.Equal(asOf) {
			out = append(out, r)
		}
	}
	return out
}

// sectorStateStrength extracts the dominant state + max strength across the per-ETF
// SectorRotationIntent slice carried by one IntentRecord. The sector adapter emits
// ONE record per timestamp whose payload is the []SectorRotationIntent slice; for
// the "all no_setup" assertion we need the per-ETF view, so we return the worst
// (no_setup if all are no_setup) state and the max strength.
func sectorStateStrength(t *testing.T, rec livengine.IntentRecord) (state string, maxStrength float64) {
	t.Helper()
	b, err := json.Marshal(rec.Payload)
	require.NoError(t, err)
	var rows []struct {
		State    string  `json:"state"`
		Strength float64 `json:"strength"`
	}
	require.NoError(t, json.Unmarshal(b, &rows))
	require.NotEmpty(t, rows)
	allNoSetup := true
	for _, r := range rows {
		if r.State != string(domain.StateNoSetup) {
			allNoSetup = false
		}
		if r.Strength > maxStrength {
			maxStrength = r.Strength
		}
	}
	if allNoSetup {
		return string(domain.StateNoSetup), maxStrength
	}
	// Return any non-no_setup state to signal "actionable".
	for _, r := range rows {
		if r.State != string(domain.StateNoSetup) {
			return r.State, maxStrength
		}
	}
	return string(domain.StateNoSetup), maxStrength
}
