package indicators

import "math"

// NaN is the canonical undefined/warmup value. Every rolling primitive emits
// NaN for indices that lack a full window (min_periods == window). Callers must
// use math.IsNaN to test, since NaN != NaN.
var NaN = math.NaN()

// SMA computes the simple moving average over `window` bars
// (rolling(window).mean() with min_periods == window).
//
// Output length == len(x). Indices [0, window-2] are NaN (warmup); index i>=
// window-1 is the arithmetic mean of x[i-window+1 .. i]. A window that contains
// any NaN yields NaN at that index (NaN propagation).
//
// window <= 0 panics (programmer error); window == 1 is the identity (mean of a
// single value).
func SMA(x []float64, window int) []float64 {
	if window <= 0 {
		panic("indicators: SMA window must be > 0")
	}
	out := make([]float64, len(x))
	for i := range out {
		out[i] = NaN
	}
	if len(x) < window {
		return out
	}
	// Running sum with NaN tracking. We recompute lazily when a NaN passes
	// through the window so propagation is exact rather than poisoning the
	// accumulator forever.
	for i := window - 1; i < len(x); i++ {
		sum := 0.0
		hasNaN := false
		for j := i - window + 1; j <= i; j++ {
			v := x[j]
			if math.IsNaN(v) {
				hasNaN = true
				break
			}
			sum += v
		}
		if hasNaN {
			out[i] = NaN
		} else {
			out[i] = sum / float64(window)
		}
	}
	return out
}

// RollingSum is the rolling(window).sum() with min_periods == window: NaN
// warmup for the first window-1 indices, then the sum of the trailing window.
// NaN inside the window propagates to NaN.
func RollingSum(x []float64, window int) []float64 {
	if window <= 0 {
		panic("indicators: RollingSum window must be > 0")
	}
	out := make([]float64, len(x))
	for i := range out {
		out[i] = NaN
	}
	if len(x) < window {
		return out
	}
	for i := window - 1; i < len(x); i++ {
		sum := 0.0
		hasNaN := false
		for j := i - window + 1; j <= i; j++ {
			if math.IsNaN(x[j]) {
				hasNaN = true
				break
			}
			sum += x[j]
		}
		if hasNaN {
			out[i] = NaN
		} else {
			out[i] = sum
		}
	}
	return out
}

// RollingStd is the rolling(window).std(ddof=ddof).
//
// ddof=1 gives the sample std (divide by N-1); pass ddof=0 for the population
// std (divide by N) used by some call sites. Output is NaN for the warmup
// window and whenever window-ddof <= 0 (e.g. window==1, ddof==1). NaN inside
// the window propagates.
//
// The two-pass mean/variance formulation is numerically stable to within the
// 1e-9 golden tolerance.
func RollingStd(x []float64, window, ddof int) []float64 {
	if window <= 0 {
		panic("indicators: RollingStd window must be > 0")
	}
	out := make([]float64, len(x))
	for i := range out {
		out[i] = NaN
	}
	if len(x) < window {
		return out
	}
	denom := float64(window - ddof)
	for i := window - 1; i < len(x); i++ {
		// First pass: mean (with NaN propagation).
		sum := 0.0
		hasNaN := false
		for j := i - window + 1; j <= i; j++ {
			if math.IsNaN(x[j]) {
				hasNaN = true
				break
			}
			sum += x[j]
		}
		if hasNaN || denom <= 0 {
			out[i] = NaN
			continue
		}
		mean := sum / float64(window)
		// Second pass: sum of squared deviations.
		ss := 0.0
		for j := i - window + 1; j <= i; j++ {
			d := x[j] - mean
			ss += d * d
		}
		out[i] = math.Sqrt(ss / denom)
	}
	return out
}

// RollingMax is the rolling(window).max() with min_periods == window. NaN
// warmup, then the max of the trailing window; NaN in the window propagates to
// NaN.
func RollingMax(x []float64, window int) []float64 {
	return rollingExtremum(x, window, true)
}

// RollingMin is the rolling(window).min() with min_periods == window. NaN
// warmup, then the min of the trailing window; NaN propagates.
func RollingMin(x []float64, window int) []float64 {
	return rollingExtremum(x, window, false)
}

func rollingExtremum(x []float64, window int, wantMax bool) []float64 {
	if window <= 0 {
		panic("indicators: rolling extremum window must be > 0")
	}
	out := make([]float64, len(x))
	for i := range out {
		out[i] = NaN
	}
	if len(x) < window {
		return out
	}
	for i := window - 1; i < len(x); i++ {
		var ext float64
		hasNaN := false
		first := true
		for j := i - window + 1; j <= i; j++ {
			v := x[j]
			if math.IsNaN(v) {
				hasNaN = true
				break
			}
			if first {
				ext = v
				first = false
				continue
			}
			if wantMax {
				if v > ext {
					ext = v
				}
			} else {
				if v < ext {
					ext = v
				}
			}
		}
		if hasNaN {
			out[i] = NaN
		} else {
			out[i] = ext
		}
	}
	return out
}

// Max returns the maximum of a non-empty slice (pandas `Series.max()`, which
// skips NaN by default — skipna=True). Returns NaN for an empty slice or a
// slice that is entirely NaN.
func Max(x []float64) float64 {
	out := NaN
	for _, v := range x {
		if math.IsNaN(v) {
			continue
		}
		if math.IsNaN(out) || v > out {
			out = v
		}
	}
	return out
}

// Min returns the minimum of a non-empty slice (pandas `Series.min()`,
// skipna=True). Returns NaN for empty or all-NaN input.
func Min(x []float64) float64 {
	out := NaN
	for _, v := range x {
		if math.IsNaN(v) {
			continue
		}
		if math.IsNaN(out) || v < out {
			out = v
		}
	}
	return out
}

// Mean returns the arithmetic mean of a slice, skipping NaN (pandas
// `Series.mean()`, skipna=True). Empty / all-NaN input returns NaN.
func Mean(x []float64) float64 {
	sum := 0.0
	n := 0
	for _, v := range x {
		if math.IsNaN(v) {
			continue
		}
		sum += v
		n++
	}
	if n == 0 {
		return NaN
	}
	return sum / float64(n)
}
