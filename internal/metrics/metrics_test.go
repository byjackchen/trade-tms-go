package metrics

import (
	"math"
	"testing"
)

// relClose asserts a and b agree to <= 1e-12 relative (spec §13.1), with an
// absolute fallback near zero.
func relClose(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.IsNaN(got) != math.IsNaN(want) {
		t.Fatalf("%s: got %v want %v (NaN mismatch)", name, got, want)
	}
	if want == 0 {
		if math.Abs(got) > 1e-12 {
			t.Fatalf("%s: got %v want 0", name, got)
		}
		return
	}
	rel := math.Abs(got-want) / math.Abs(want)
	if rel > 1e-12 {
		t.Fatalf("%s: got %v want %v (rel %.3e > 1e-12)", name, got, want, rel)
	}
}

// goldenVectors are produced by the Python reference (research.metrics) over
// the §1 test curves and a few randomized ones; see the build harness in the
// builder notes. Each asserts sharpe/calmar/mdd parity to 1e-12 relative.
var goldenVectors = []struct {
	name     string
	curve    []float64
	sharpe   float64
	calmar   float64
	mdd      float64
	nReturns int
}{
	{"flat", []float64{100, 100, 100, 100}, 0.0, 0.0, 0.0, 3},
	{"monotonic", []float64{100, 110, 120, 130}, 212.98212276006774, 372598921650.6072, 0.0, 3},
	{"dd_example", []float64{100000, 110000, 99000, 120000}, 8.694648607715937, 44794487.38824558, -10.0, 3},
	{"losing", []float64{100, 98, 95, 90}, -40.19859335499396, -9.998566588802033, -10.0, 3},
	{"single", []float64{100}, 0.0, 0.0, 0.0, 0},
	{"two", []float64{100, 105}, 0.0, 21862578.363222267, 0.0, 1},
	{"empty", []float64{}, 0.0, 0.0, 0.0, 0},
	{"zero_prev", []float64{0, 100, 110, 90}, -4.608728090241544, 0.0, -18.181818181818183, 2},
	{"mixed", []float64{100, 101.5, 99.2, 103.7, 98.1, 107.4}, 4.802388404380384, 657.8866769427062, -5.400192864030866, 5},
	{"wipeout", []float64{100, 50, 0}, -47.62352359916263, -1.0, -100.0, 2},
	// Finding #1 regression: bit-identical non-binary-exact returns
	// [0.1,0.1,0.1] -> mean exactly 0.1, pstdev exactly 0 -> Sharpe 0.0.
	{"const_rate_10pct", []float64{100000, 110000, 121000, 133100}, 0.0, 2697470226675.794, 0.0, 3},
}

func TestGoldenVectors(t *testing.T) {
	for _, tc := range goldenVectors {
		t.Run(tc.name, func(t *testing.T) {
			relClose(t, "sharpe", Sharpe(tc.curve), tc.sharpe)
			relClose(t, "calmar", Calmar(tc.curve), tc.calmar)
			relClose(t, "mdd", MaxDrawdownPct(tc.curve), tc.mdd)
			if got := len(Returns(tc.curve)); got != tc.nReturns {
				t.Fatalf("returns count: got %d want %d", got, tc.nReturns)
			}
		})
	}
}

func TestReturnsZeroPrevDropsPair(t *testing.T) {
	// A zero previous value drops the pair entirely; it must NOT emit a 0.
	r := Returns([]float64{0, 100, 110})
	if len(r) != 1 {
		t.Fatalf("got %d returns, want 1 (zero-prev pair dropped)", len(r))
	}
	relClose(t, "ret", r[0], 0.1)
}

func TestSharpeEdgeCases(t *testing.T) {
	cases := []struct {
		name  string
		curve []float64
		want  float64
	}{
		{"empty", nil, 0.0},
		{"single", []float64{100}, 0.0},
		{"two_points_one_return", []float64{100, 110}, 0.0}, // <2 returns -> 0
		{"flat", []float64{100, 100, 100}, 0.0},             // vol==0 -> 0
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Sharpe(tc.curve); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestMaxDrawdownEdgeCases(t *testing.T) {
	if got := MaxDrawdownPct(nil); got != 0 {
		t.Fatalf("empty: got %v want 0", got)
	}
	if got := MaxDrawdownPct([]float64{100, 110, 120}); got != 0 {
		t.Fatalf("monotonic-up: got %v want 0", got)
	}
	// A zero peak is skipped (no division by zero, no NaN/Inf).
	got := MaxDrawdownPct([]float64{0, 0, 100, 50})
	if math.IsNaN(got) || math.IsInf(got, 0) {
		t.Fatalf("zero-peak skip produced %v", got)
	}
	relClose(t, "mdd", got, -50.0)
	// Result is always non-positive.
	if MaxDrawdownPct([]float64{100, 90, 80}) > 0 {
		t.Fatal("max drawdown must be non-positive")
	}
}

func TestCalmarEdgeCases(t *testing.T) {
	// Too short / zero start -> 0.
	if got := Calmar([]float64{100}); got != 0 {
		t.Fatalf("single: got %v want 0", got)
	}
	if got := Calmar([]float64{0, 100, 110}); got != 0 {
		t.Fatalf("zero start: got %v want 0", got)
	}
	// Total wipeout -> exactly -1.0.
	if got := Calmar([]float64{100, 0}); got != -1.0 {
		t.Fatalf("wipeout: got %v want -1", got)
	}
	if got := Calmar([]float64{100, 50, 0}); got != -1.0 {
		t.Fatalf("wipeout-3pt: got %v want -1", got)
	}
	// Zero-drawdown positive growth -> ann / 0.01 (synthetic 1% DD floor).
	c := []float64{100, 110, 120}
	tr := 120.0/100.0 - 1.0
	years := math.Max(float64(len(c)-1)/252, 1.0/252)
	ann := math.Pow(1+tr, 1/years) - 1
	relClose(t, "zero-dd", Calmar(c), ann/0.01)
	// Zero-drawdown but non-positive growth -> 0.
	if got := Calmar([]float64{100, 100}); got != 0 {
		t.Fatalf("zero-dd flat: got %v want 0", got)
	}
}

func TestObjectivesOrdering(t *testing.T) {
	m := BacktestMetrics{Sharpe: 1.5, Calmar: 2.5}
	s, c := m.Objectives()
	if s != 1.5 || c != 2.5 {
		t.Fatalf("objectives = (%v,%v), want (1.5, 2.5)", s, c)
	}
}

func TestCompute(t *testing.T) {
	curve := []float64{100000, 110000, 99000, 120000}
	m := Compute(curve, 100000, 120000, Counts{NumOrders: 4, NumFilledOrders: 3, NumRejectedOrders: 1, NumPositions: 2})
	if m.FinalBalanceUSD != 120000 {
		t.Fatalf("final: %v", m.FinalBalanceUSD)
	}
	if m.TotalPnLUSD != 20000 {
		t.Fatalf("pnl: %v", m.TotalPnLUSD)
	}
	relClose(t, "mdd", m.MaxDrawdownPct, -10.0)
	if m.NumOrders != 4 || m.NumFilledOrders != 3 || m.NumRejectedOrders != 1 || m.NumPositions != 2 {
		t.Fatalf("counters wrong: %+v", m)
	}
}

func TestExactMeanMatchesCPython(t *testing.T) {
	// Exact-rational mean reproduces CPython statistics.mean bit-for-bit. The
	// load-bearing case: bit-identical non-binary-exact returns must mean to
	// EXACTLY 0.1 (the float), not 0.10000000000000002, so that the deviations
	// are exactly 0 and pstdev==0 (covered by TestSharpeBitIdenticalReturns).
	cases := []struct {
		xs   []float64
		want float64
	}{
		{[]float64{0.1, 0.2, 0.3}, 0.2},
		{[]float64{0.1, 0.1, 0.1}, 0.1}, // must be EXACTLY 0.1
		{[]float64{0.5, 0.5, 0.5}, 0.5},
	}
	for _, c := range cases {
		if got := exactMean(c.xs); got != c.want {
			t.Fatalf("exactMean(%v) = %v, want exactly %v", c.xs, got, c.want)
		}
	}
}

func TestExactPstdevZeroOnBitIdentical(t *testing.T) {
	// statistics.pstdev of bit-identical values is exactly 0.0 — the property
	// that makes the Sharpe vol==0 guard fire (finding #1). A float64
	// compensated sum would give ~1.4e-17 here.
	for _, xs := range [][]float64{{0.1, 0.1, 0.1}, {0.5, 0.5, 0.5}, {0.08333333333333333, 0.08333333333333333}} {
		if got := exactPstdev(xs); got != 0.0 {
			t.Fatalf("exactPstdev(%v) = %v, want exactly 0.0", xs, got)
		}
	}
}

// TestSharpeBitIdenticalReturns is the regression for finding #1: a curve whose
// per-period returns are all the same non-binary-exact float (a constant
// compounding rate — exactly the hyperopt fold-stitch path stitched[-1]*(1+ret))
// must yield Sharpe 0.0, matching CPython compute_sharpe. The previous Neumaier
// mean made vol != 0 and produced ~1.14e17.
func TestSharpeBitIdenticalReturns(t *testing.T) {
	cases := []struct {
		name  string
		curve []float64
	}{
		// returns [0.1,0.1,0.1] — 0.1 is NOT binary-exact (the catastrophic case).
		{"rate_10pct", []float64{100000, 110000, 121000, 133100}},
		// returns [0.5,0.5,0.5] — 0.5 IS binary-exact (escaped even before).
		{"rate_50pct", []float64{100000, 150000, 225000, 337500}},
		// Many-point exactly-representable geometric curve (×1.5 each step):
		// every return is exactly 0.5, vol must be exactly 0.
		{"rate_50pct_long", []float64{100000, 150000, 225000, 337500, 506250, 759375}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Sharpe(tc.curve); got != 0.0 {
				t.Fatalf("Sharpe(%v) = %v, want exactly 0.0", tc.curve, got)
			}
			if got := Volatility(tc.curve); got != 0.0 {
				t.Fatalf("Volatility = %v, want exactly 0.0", got)
			}
		})
	}
}

// TestSharpeStitchCompoundParity asserts Go matches CPython even when the
// fold-stitch recurrence stitched[-1]*(1+r) does NOT produce bit-identical
// returns (floating error accumulates): both languages yield the same large
// value. Captured from research.metrics.compute_sharpe on the same curve.
func TestSharpeStitchCompoundParity(t *testing.T) {
	curve := []float64{100000.0}
	for i := 0; i < 5; i++ {
		curve = append(curve, curve[len(curve)-1]*1.1)
	}
	// Python: compute_sharpe(curve) == 2.0968316539602468e+16
	relClose(t, "sharpe", Sharpe(curve), 2.0968316539602468e+16)
}
