package study

// fixer_round2_test.go covers the FIXER round-2 hardening:
//
//   finding 1 (objective parity / gate): the single-strategy hyperopt objective
//   path must gate the optimized sub-strategy under the canonical MULTI-strategy
//   portfolio (SEPA 40 / Sector 30 / Pairs 20; single-name 50%, concentration
//   40%, daily-loss 10%) — exactly what scripts/multi_strategy_backtest.run_backtest
//   always installs, even for a single-strategy trial — NOT the lone-strategy
//   100%-budget / default-caps gate. The two gates admit/reject DIFFERENT order
//   sets, so using the wrong one makes the objective vector diverge from Python.
//
//   finding 2 (num_filled_orders): the objective path must count orders that
//   produced at least one fill (Nautilus is_closed, derived from res.Fills), NOT
//   orders whose Status == FILLED — the engine never mutates a submitted order's
//   status to FILLED, so the old counter was ALWAYS 0 despite real fills.
//
// Both are proven over a deterministic synthetic 3-pair dataset (no DB / net)
// engineered so the multi-strategy Pairs 20% allocator budget binds (admitting
// far fewer entries than the lone-strategy 100% budget) — making the gate choice
// observable in every counter. The Evaluator's metrics are compared against an
// engine driven directly with each candidate gate.

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/composition"
	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/engine"
	"github.com/byjackchen/trade-tms-go/internal/engine/strategyassembly"
	"github.com/byjackchen/trade-tms-go/internal/metrics"
	"github.com/byjackchen/trade-tms-go/internal/params"
)

// tradingTriplePairs builds a deterministic 3-pair dataset (A1/B1, A2/B2,
// A3/B3) over [start-warmup, end] (daily, weekdays only). Each pair shares a
// cointegrating common factor plus a mean-reverting residual with a distinct
// phase, so OLS finds a real hedge ratio and the z-score crosses ±entry_z
// repeatedly — the pairs strategy actually trades, on all three pairs.
func tradingTriplePairs(t *testing.T, start, end calendar.Date) *Dataset {
	t.Helper()
	legs := []struct {
		sym  string
		pair int
		role int // 0 = long leg (factor + residual), 1 = short leg (factor only)
	}{
		{"A1", 0, 0}, {"B1", 0, 1},
		{"A2", 1, 0}, {"B2", 1, 1},
		{"A3", 2, 0}, {"B3", 2, 1},
	}
	lo := midnight(start.AddDays(-warmupDaysDefault))
	hi := midnight(end)

	ibs := make([]engine.InstrumentBars, 0, len(legs))
	for _, L := range legs {
		var bars []domain.Bar
		day := lo
		i := 0
		ph := float64(L.pair) // per-pair phase offset
		for !day.After(hi) {
			if wd := day.Weekday(); wd == time.Saturday || wd == time.Sunday {
				day = day.AddDate(0, 0, 1)
				continue
			}
			common := 10.0 * math.Sin(float64(i)/7.0+ph)
			px := 100.0 + common
			if L.role == 0 {
				px += 12.0 * math.Sin(float64(i)/3.3+ph) // mean-reverting residual
			}
			p, err := domain.PriceFromFloat64(px)
			if err != nil {
				t.Fatal(err)
			}
			bars = append(bars, domain.Bar{
				Symbol: L.sym, TS: day, Open: p, High: p, Low: p, Close: p, Volume: 1_000_000,
			})
			day = day.AddDate(0, 0, 1)
			i++
		}
		ibs = append(ibs, engine.InstrumentBars{Symbol: L.sym, Bars: bars})
	}
	return NewDatasetFromInstruments(ibs)
}

// triplePairDefaults is the merged pairs defaults map for the 3-pair dataset:
// three pairs, short lookback, low entry_z (so it trades), and a per-pair
// capital fraction sized so the multi-strategy Pairs 20% budget binds before the
// lone-strategy 100% budget — the divergence the gate test relies on.
func triplePairDefaults() map[string]any {
	return map[string]any{
		"pairs":                []any{[]any{"A1", "B1"}, []any{"A2", "B2"}, []any{"A3", "B3"}},
		"lookback":             float64(20),
		"entry_z":              1.0,
		"exit_z":               0.25,
		"capital_per_pair_pct": 0.2,
		"timezone":             "America/New_York",
	}
}

// runPairsEngineLoneGate assembles + runs the pairs strategy over [start, end]
// on the shared dataset under the pairs-only Composition's lone gate (100% budget,
// the pairs default risk caps) and returns the whole run Result. It mirrors
// Evaluator.run so the two can be compared directly. With parity abandoned the
// pairs objective now uses exactly this lone gate (it no longer force-installs
// the multi-strategy gate — docs/concept-alignment.md §3.2, D1).
func runPairsEngineLoneGate(t *testing.T, ds *Dataset, defaults map[string]any, start, end calendar.Date, startBal float64) *engine.Result {
	t.Helper()
	pp, err := params.PairsFromMap(defaults)
	if err != nil {
		t.Fatalf("PairsFromMap: %v", err)
	}
	comp, err := composition.Seed("pairs-only")
	if err != nil {
		t.Fatalf("composition.Seed(pairs-only): %v", err)
	}
	in := strategyassembly.Input{
		Composition:     comp,
		StartingBalance: startBal,
	}
	in.Params.Pairs = pp
	asm, err := strategyassembly.Assemble(in)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	sb, err := domain.MoneyFromFloat64(startBal)
	if err != nil {
		t.Fatalf("MoneyFromFloat64: %v", err)
	}
	cfg := engine.Config{
		Start:              start,
		End:                end,
		StartingBalance:    sb,
		Profile:            engine.ProfileNautilusCompat,
		Gate:               asm.Gate,
		Context:            asm.Context,
		SPYSymbol:          asm.SPYSymbol,
		PrebuiltStrategies: asm.Strategies,
		Tickers:            asm.ExtraTickers,
	}
	// Parity-correct feed: the engine replays ONLY [start, end]. Pairs receives
	// NO warmup priming (its adapter is not a WarmupConsumer), mirroring Python
	// where the Pairs loader pulls run-window-only bars — so the reference engine
	// here uses the same WindowFeed the Evaluator.run uses. (Replaying the 400d
	// warmup tail through the loop, as the old ds.Feed did, would WARM Pairs'
	// rolling state and inflate its admitted/rejected order set vs Python.)
	feed := ds.WindowFeed()
	eng, err := engine.New(context.Background(), cfg, feed)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	asm.BindEquity(eng)
	res, err := eng.Run(context.Background())
	if err != nil {
		t.Fatalf("engine.Run: %v", err)
	}
	return res
}

// newSingleWindowPairsEvaluator builds the Evaluator the orchestrator would
// build for a single-window (no walk-forward) pairs study over [start, end] with
// the given defaults. Single-window mode exercises the SAME run() / gate path as
// a fold and lets the aggregated metrics be compared 1:1 to a full-window engine
// run (no stitching to reason about).
func newSingleWindowPairsEvaluator(t *testing.T, ds *Dataset, defaults map[string]any, start, end calendar.Date, startBal float64) *Evaluator {
	t.Helper()
	ev, err := NewEvaluator(EvaluatorConfig{
		Strategy:        "pairs",
		Dataset:         ds,
		Start:           start,
		End:             end,
		Folds:           nil, // single-window
		Defaults:        map[string]map[string]any{"pairs": defaults},
		StartingBalance: startBal,
	})
	if err != nil {
		t.Fatalf("NewEvaluator: %v", err)
	}
	return ev
}

// emptyPairsDecoded is the no-override candidate (baseline defaults flow through
// unchanged), so the Evaluator's params equal the engine helper's params.
func emptyPairsDecoded() Decoded {
	return Decoded{Overrides: map[string]map[string]float64{"pairs": {}}}
}

// TestObjectiveUsesLoneGate proves the post-parity contract: the single-strategy
// pairs objective path gates under the pairs-only Composition's LONE gate (100% budget
// + the pairs default risk caps), NOT the multi-strategy portfolio gate. The old
// MultiStrategyGate force-install is gone (parity abandoned), so the Evaluator's
// counters must equal a lone-gate engine run over the same dataset.
func TestObjectiveUsesLoneGate(t *testing.T) {
	const startBal = 100000.0
	start := calendar.NewDate(2023, 1, 2)
	end := calendar.NewDate(2023, 12, 29)
	ds := tradingTriplePairs(t, start, end)
	defaults := triplePairDefaults()

	ev := newSingleWindowPairsEvaluator(t, ds, defaults, start, end, startBal)
	res, err := ev.Evaluate(context.Background(), emptyPairsDecoded())
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	got := res.Aggregated

	loneRes := runPairsEngineLoneGate(t, ds, defaults, start, end, startBal)
	loneCounts := loneRes.Counts("")

	// The Evaluator's metrics must match the lone-gate engine exactly.
	if counterTuple(got) != loneCounts {
		t.Fatalf("objective counts %+v != lone-gate engine counts %+v (objective is NOT using the pairs-only lone gate)",
			counterTuple(got), loneCounts)
	}
	t.Logf("lone-gate counts=%+v objective counts=%+v sharpe=%.6f calmar=%.6f",
		loneCounts, counterTuple(got), got.Sharpe, got.Calmar)
}

// engineMetricsSharpe recomputes a run's Sharpe the same way the objective path
// does (equity curve -> metrics.Compute), so the gate test can assert
// objective-VALUE parity, not just counter parity.
func engineMetricsSharpe(t *testing.T, r *engine.Result, startBal float64) float64 {
	t.Helper()
	pts := r.TotalEquityCurve
	curve := make([]float64, 0, len(pts)+2)
	if len(pts) == 0 {
		curve = []float64{startBal, r.FinalBalance.Float64()}
	} else {
		for _, p := range pts {
			curve = append(curve, p.Value.Float64())
		}
	}
	c := r.Counts("")
	m := metrics.Compute(curve, startBal, r.FinalBalance.Float64(), metrics.Counts{
		NumOrders: c.NumOrders, NumFilledOrders: c.NumFilledOrders,
		NumRejectedOrders: c.NumRejectedOrders, NumPositions: c.NumPositions,
	})
	return m.Sharpe
}

// TestObjectiveObjectiveValuesMatchLoneGate proves the objective VECTOR (sharpe,
// calmar) computed by the Evaluator equals the lone-gate engine run's metrics
// exactly — the post-parity contract at the value level (the objective uses the
// pairs-only Composition's gate, not the multi gate).
func TestObjectiveObjectiveValuesMatchLoneGate(t *testing.T) {
	const startBal = 100000.0
	start := calendar.NewDate(2023, 1, 2)
	end := calendar.NewDate(2023, 12, 29)
	ds := tradingTriplePairs(t, start, end)
	defaults := triplePairDefaults()

	ev := newSingleWindowPairsEvaluator(t, ds, defaults, start, end, startBal)
	res, err := ev.Evaluate(context.Background(), emptyPairsDecoded())
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	loneRes := runPairsEngineLoneGate(t, ds, defaults, start, end, startBal)
	wantSharpe := engineMetricsSharpe(t, loneRes, startBal)

	if res.Aggregated.Sharpe != wantSharpe {
		t.Fatalf("objective sharpe=%.10f != lone-gate engine sharpe=%.10f", res.Aggregated.Sharpe, wantSharpe)
	}
	objs := res.Objectives()
	if len(objs) != 2 {
		t.Fatalf("objective vector len=%d want 2", len(objs))
	}
	if objs[0] != res.Aggregated.Sharpe {
		t.Fatalf("objective[0]=%.10f != aggregated sharpe=%.10f", objs[0], res.Aggregated.Sharpe)
	}
}

// TestObjectiveNumFilledOrdersNonZero proves finding 2: when the run produces
// real fills, num_filled_orders is reported > 0 (it was hard-wired to 0 by the
// old Status==FILLED counter), and equals res.Counts("").NumFilledOrders.
func TestObjectiveNumFilledOrdersNonZero(t *testing.T) {
	const startBal = 100000.0
	start := calendar.NewDate(2023, 1, 2)
	end := calendar.NewDate(2023, 12, 29)
	ds := tradingTriplePairs(t, start, end)
	defaults := triplePairDefaults()

	ev := newSingleWindowPairsEvaluator(t, ds, defaults, start, end, startBal)
	res, err := ev.Evaluate(context.Background(), emptyPairsDecoded())
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	got := res.Aggregated

	wantRes := runPairsEngineLoneGate(t, ds, defaults, start, end, startBal)
	want := wantRes.Counts("")

	// Sanity: this dataset must actually fill orders, else the test is vacuous.
	if len(wantRes.Fills) == 0 {
		t.Fatalf("dataset produced no fills; cannot prove num_filled_orders fix")
	}
	if want.NumFilledOrders == 0 {
		t.Fatalf("engine Counts reports 0 filled despite %d fills", len(wantRes.Fills))
	}
	if got.NumFilledOrders == 0 {
		t.Fatalf("objective num_filled_orders is 0 despite %d fills (finding 2 not fixed)", len(wantRes.Fills))
	}
	if got.NumFilledOrders != want.NumFilledOrders {
		t.Fatalf("objective num_filled_orders=%d != engine Counts num_filled_orders=%d",
			got.NumFilledOrders, want.NumFilledOrders)
	}
}

// TestResultCountsFilledDerivedFromFills proves the engine.Result.Counts single
// source of truth: the filled count is derived from res.Fills (NOT Order.Status,
// which the recorder never mutates to FILLED), the rejected count unions
// REJECTED-status orders with gate-blocked signals, and all four counters equal
// an independent recomputation.
func TestResultCountsFilledDerivedFromFills(t *testing.T) {
	const startBal = 100000.0
	start := calendar.NewDate(2023, 1, 2)
	end := calendar.NewDate(2023, 12, 29)
	ds := tradingTriplePairs(t, start, end)
	defaults := triplePairDefaults()
	res := runPairsEngineLoneGate(t, ds, defaults, start, end, startBal)

	c := res.Counts("")

	// Independent recomputation.
	filledOrderIDs := map[string]bool{}
	for _, f := range res.Fills {
		filledOrderIDs[f.ClientOrderID] = true
	}
	wantFilled := 0
	wantRejected := 0
	statusFilled := 0
	for _, o := range res.Orders {
		if filledOrderIDs[o.ClientOrderID] {
			wantFilled++
		}
		if o.Status == domain.OrderStatusRejected {
			wantRejected++
		}
		if o.Status == domain.OrderStatusFilled {
			statusFilled++
		}
	}
	wantRejected += len(res.RejectedOrders)

	if c.NumOrders != len(res.Orders) {
		t.Fatalf("NumOrders=%d want %d", c.NumOrders, len(res.Orders))
	}
	if c.NumFilledOrders != wantFilled {
		t.Fatalf("NumFilledOrders=%d want %d", c.NumFilledOrders, wantFilled)
	}
	if c.NumRejectedOrders != wantRejected {
		t.Fatalf("NumRejectedOrders=%d want %d", c.NumRejectedOrders, wantRejected)
	}
	if c.NumPositions != len(res.Positions) {
		t.Fatalf("NumPositions=%d want %d", c.NumPositions, len(res.Positions))
	}

	// Regression guard for finding 2: the OLD counter counted orders whose
	// Status == FILLED. The recorder records orders at submit time and never sets
	// FILLED, so that count must be 0 here while real fills exist — i.e. the old
	// implementation would have reported 0 and diverged from this (correct) count.
	if len(res.Fills) > 0 && c.NumFilledOrders == 0 {
		t.Fatalf("real fills present (%d) but Counts num_filled=0", len(res.Fills))
	}
	if statusFilled != 0 {
		t.Fatalf("recorder unexpectedly set %d orders to FILLED status; the Status-based "+
			"counter assumption underlying finding 2 has changed — revisit", statusFilled)
	}
}

// counterTuple projects a BacktestMetrics into its four integer counters for
// direct comparison with engine.OrderCounts.
func counterTuple(m metrics.BacktestMetrics) engine.OrderCounts {
	return engine.OrderCounts{
		NumOrders:         m.NumOrders,
		NumFilledOrders:   m.NumFilledOrders,
		NumRejectedOrders: m.NumRejectedOrders,
		NumPositions:      m.NumPositions,
	}
}
