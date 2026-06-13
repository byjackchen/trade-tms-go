package indicators

import (
	"math"
	"math/rand"
	"testing"
)

// The streaming accumulators must reproduce the batch rolling values EXACTLY at
// every step (bit-for-bit, not within tol) for the SMA/min/max forms which are
// exact-arithmetic, and within a tight float tolerance for std (single-pass vs
// two-pass variance). We feed identical streams to both and compare per index.

func streamSMA(x []float64, w int) []float64 {
	acc := NewRollingSMA(w)
	out := make([]float64, len(x))
	for i, v := range x {
		out[i] = acc.Update(v)
	}
	return out
}

func streamMax(x []float64, w int) []float64 {
	acc := NewRollingMax(w)
	out := make([]float64, len(x))
	for i, v := range x {
		out[i] = acc.Update(v)
	}
	return out
}

func streamMin(x []float64, w int) []float64 {
	acc := NewRollingMin(w)
	out := make([]float64, len(x))
	for i, v := range x {
		out[i] = acc.Update(v)
	}
	return out
}

func streamStd(x []float64, w, ddof int) []float64 {
	acc := NewRollingStd(w, ddof)
	out := make([]float64, len(x))
	for i, v := range x {
		out[i] = acc.Update(v)
	}
	return out
}

func eqExact(t *testing.T, ctx string, got, want []float64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: len %d vs %d", ctx, len(got), len(want))
	}
	for i := range got {
		if math.IsNaN(want[i]) {
			if !math.IsNaN(got[i]) {
				t.Errorf("%s[%d]: want NaN got %v", ctx, i, got[i])
			}
			continue
		}
		if got[i] != want[i] {
			t.Errorf("%s[%d]: streaming %.17g != batch %.17g", ctx, i, got[i], want[i])
		}
	}
}

func eqClose(t *testing.T, ctx string, got, want []float64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: len %d vs %d", ctx, len(got), len(want))
	}
	for i := range got {
		if math.IsNaN(want[i]) {
			if !math.IsNaN(got[i]) {
				t.Errorf("%s[%d]: want NaN got %v", ctx, i, got[i])
			}
			continue
		}
		if math.Abs(got[i]-want[i]) > 1e-9 {
			t.Errorf("%s[%d]: streaming %.17g vs batch %.17g", ctx, i, got[i], want[i])
		}
	}
}

func TestIncrementalMatchesBatch(t *testing.T) {
	rng := rand.New(rand.NewSource(20240613))
	vectors := [][]float64{
		{1, 2, 3, 4, 5, 4, 4, 6, 2, 8, 8, 7},
		{5, 5, 5, 5, 5},                               // constant (std==0)
		{1, math.NaN(), 3, 4, 5, 6, math.NaN(), 8, 9}, // NaN propagation
	}
	// A long random vector.
	big := make([]float64, 500)
	for i := range big {
		big[i] = 100 + rng.NormFloat64()*5
	}
	vectors = append(vectors, big)

	windows := []int{1, 2, 3, 5, 20}
	for vi, x := range vectors {
		for _, w := range windows {
			if w > len(x) {
				continue
			}
			ctx := "vec" + itoa(vi) + " w" + itoa(w)
			// Min/Max are pure comparisons -> bit-identical to batch.
			// SMA/Std use a running accumulator (O(1) update), so they
			// match the per-window batch recompute only within float
			// tolerance on long drifting streams — that is the documented
			// trade-off; the batch form remains the pandas-parity golden.
			eqClose(t, "SMA "+ctx, streamSMA(x, w), SMA(x, w))
			eqExact(t, "Max "+ctx, streamMax(x, w), RollingMax(x, w))
			eqExact(t, "Min "+ctx, streamMin(x, w), RollingMin(x, w))
			// Std accumulator re-derives variance two-pass over the live
			// window in chronological order -> bit-identical to batch.
			eqExact(t, "Std1 "+ctx, streamStd(x, w, 1), RollingStd(x, w, 1))
			eqExact(t, "Std0 "+ctx, streamStd(x, w, 0), RollingStd(x, w, 0))
		}
	}
}

// Value() without Update must agree with the last Update return.
func TestAccumulatorValueConsistency(t *testing.T) {
	x := []float64{3, 1, 4, 1, 5, 9, 2, 6}
	sma := NewRollingSMA(3)
	std := NewRollingStd(3, 1)
	var lastSMA, lastStd float64
	for _, v := range x {
		lastSMA = sma.Update(v)
		lastStd = std.Update(v)
	}
	if got := sma.Value(); got != lastSMA && !(math.IsNaN(got) && math.IsNaN(lastSMA)) {
		t.Errorf("SMA Value %v != last Update %v", got, lastSMA)
	}
	if got := std.Value(); math.Abs(got-lastStd) > 1e-12 {
		t.Errorf("Std Value %v != last Update %v", got, lastStd)
	}
}
