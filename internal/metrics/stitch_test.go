package metrics

import (
	"math"
	"testing"
)

func TestStitchConcatenatesReturns(t *testing.T) {
	// Two folds, each starting fresh at 100k; stitching chains the per-period
	// returns from the recovered starting balance (fold0.final - fold0.pnl).
	curves := [][]float64{
		{100000, 101000, 102000},
		{100000, 101500, 103000},
	}
	got := Stitch(curves, 100000)
	// Golden for the stitched equity curve.
	want := []float64{100000, 101000, 102000, 103529.99999999999, 105060.0}
	if len(got) != len(want) {
		t.Fatalf("len: got %d want %d", len(got), len(want))
	}
	for i := range want {
		relClose(t, "pt", got[i], want[i])
	}
}

func TestStitchSkipsDegenerateFolds(t *testing.T) {
	curves := [][]float64{
		{100000},         // single point -> contributes nothing
		{},               // empty -> contributes nothing
		{100000, 110000}, // one return +10%
	}
	got := Stitch(curves, 100000)
	want := []float64{100000, 110000}
	if len(got) != len(want) {
		t.Fatalf("len: got %d want %d", len(got), len(want))
	}
	relClose(t, "pt1", got[1], want[1])
}

func TestStitchAlwaysHasAtLeastOnePoint(t *testing.T) {
	got := Stitch(nil, 100000)
	if len(got) != 1 || got[0] != 100000 {
		t.Fatalf("got %v, want [100000]", got)
	}
}

func TestStitchZeroPrevSkipped(t *testing.T) {
	// A zero previous value inside a fold drops that pair (no return chained).
	got := Stitch([][]float64{{0, 100, 110}}, 100000)
	// First pair (0->100) skipped; second pair (100->110) => +10%.
	want := []float64{100000, 110000}
	if len(got) != len(want) {
		t.Fatalf("len: got %d want %d", len(got), len(want))
	}
	relClose(t, "pt1", got[1], 110000)
}

func TestAggregateFoldsRecomputesNotAverages(t *testing.T) {
	curves := [][]float64{
		{100000, 101000, 102000},
		{100000, 101500, 103000},
	}
	f0 := BacktestMetrics{FinalBalanceUSD: 102000, TotalPnLUSD: 2000, Sharpe: 1, Calmar: 1, MaxDrawdownPct: -1, NumOrders: 5, NumFilledOrders: 4, NumRejectedOrders: 1, NumPositions: 2}
	f1 := BacktestMetrics{FinalBalanceUSD: 103000, TotalPnLUSD: 3000, Sharpe: 2, Calmar: 2, MaxDrawdownPct: -2, NumOrders: 7, NumFilledOrders: 6, NumRejectedOrders: 0, NumPositions: 3}

	agg := AggregateFolds(curves, []BacktestMetrics{f0, f1})

	relClose(t, "final", agg.FinalBalanceUSD, 105060.0)
	relClose(t, "pnl", agg.TotalPnLUSD, 5060.0)
	relClose(t, "sharpe", agg.Sharpe, 79.79466624882176)
	relClose(t, "calmar", agg.Calmar, 2141.5889518128834)
	relClose(t, "mdd", agg.MaxDrawdownPct, 0.0)
	// Counters are summed across folds.
	if agg.NumOrders != 12 || agg.NumFilledOrders != 10 || agg.NumRejectedOrders != 1 || agg.NumPositions != 5 {
		t.Fatalf("counters: %+v", agg)
	}
	// Both objectives describe the same (positive) sequence -> both > 0.
	if agg.Sharpe <= 0 || agg.Calmar <= 0 {
		t.Fatal("two positive folds must yield positive sharpe and calmar")
	}
}

func TestAggregateFoldsTwoLosingFolds(t *testing.T) {
	curves := [][]float64{
		{100000, 99000, 98000},
		{100000, 99500, 97000},
	}
	f0 := BacktestMetrics{FinalBalanceUSD: 98000, TotalPnLUSD: -2000}
	f1 := BacktestMetrics{FinalBalanceUSD: 97000, TotalPnLUSD: -3000}
	agg := AggregateFolds(curves, []BacktestMetrics{f0, f1})
	if agg.Sharpe >= 0 || agg.Calmar >= 0 || agg.MaxDrawdownPct >= 0 {
		t.Fatalf("two losing folds must give negative sharpe/calmar/mdd: %+v", agg)
	}
}

func TestAggregateFoldsEmpty(t *testing.T) {
	got := AggregateFolds(nil, nil)
	if got != (BacktestMetrics{}) {
		t.Fatalf("empty: got %+v", got)
	}
}

func TestNoNaNInfInGolden(t *testing.T) {
	for _, tc := range goldenVectors {
		for _, v := range []float64{Sharpe(tc.curve), Calmar(tc.curve), MaxDrawdownPct(tc.curve)} {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				t.Fatalf("%s produced NaN/Inf: %v", tc.name, v)
			}
		}
	}
}
