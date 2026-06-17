package indicators

import "math"

// HighLow primitives back the SEPA Trend Template's 52-week high/low rules and
// the N-bar breakout windows (rolling_high / rolling_low, window 252), with the
// trend-template fall-back to the full-history extremum when the rolling window
// is not yet full.

// RollingHigh is the N-bar rolling maximum, identical to RollingMax but named
// for the SEPA call sites. Used with window=252 for the 52-week high.
func RollingHigh(high []float64, window int) []float64 { return RollingMax(high, window) }

// RollingLow is the N-bar rolling minimum.
func RollingLow(low []float64, window int) []float64 { return RollingMin(low, window) }

// FiftyTwoWeekHigh returns the last value of RollingHigh(high, 252) with the
// SEPA fall-back: when fewer than `window` bars exist the rolling value is NaN,
// and the result falls back to the full-history max (the max over high, which
// skips NaN).
//
// `window` is exposed (rather than hard-coded 252) so callers and tests can use
// the exact constant from the spec.
func FiftyTwoWeekHigh(high []float64, window int) float64 {
	if len(high) == 0 {
		return NaN
	}
	roll := RollingHigh(high, window)
	v := roll[len(roll)-1]
	if math.IsNaN(v) {
		return Max(high)
	}
	return v
}

// FiftyTwoWeekLow is the symmetric low counterpart (trend-template fall-back to
// the full-history low).
func FiftyTwoWeekLow(low []float64, window int) float64 {
	if len(low) == 0 {
		return NaN
	}
	roll := RollingLow(low, window)
	v := roll[len(roll)-1]
	if math.IsNaN(v) {
		return Min(low)
	}
	return v
}

// PctReturn is the simple percentage return of x over `window` bars:
// (x[i] - x[i-window]) / x[i-window]. This is the Sector Rotation momentum
// definition computed from the deque's first vs last element:
// (new - old) / old, where old = history[0] and new = history[-1] over a deque
// of maxlen window+1.
//
// Output length == len(x); indices [0, window-1] are NaN (insufficient
// history). A zero or NaN denominator yields NaN. Note: this is a FRACTION (not
// percent); multiply by 100 for the percent form the reasons string uses.
func PctReturn(x []float64, window int) []float64 {
	if window <= 0 {
		panic("indicators: PctReturn window must be > 0")
	}
	out := make([]float64, len(x))
	for i := range out {
		out[i] = NaN
	}
	for i := window; i < len(x); i++ {
		old := x[i-window]
		cur := x[i]
		if math.IsNaN(old) || math.IsNaN(cur) || old == 0 {
			out[i] = NaN
			continue
		}
		out[i] = (cur - old) / old
	}
	return out
}

// WindowReturn computes the Sector Rotation momentum the way the signal
// generator does at rebalance time: over a deque holding the trailing
// (lookback+1) closes, the return is (history[-1] - history[0]) / history[0].
// Given a slice that is exactly that deque snapshot, this returns the fraction
// (or NaN if the slice is too short or the base is <= 0; the `old <= 0` guard
// skips the symbol).
func WindowReturn(deque []float64) float64 {
	if len(deque) < 2 {
		return NaN
	}
	old := deque[0]
	cur := deque[len(deque)-1]
	if old <= 0 || math.IsNaN(old) || math.IsNaN(cur) {
		return NaN
	}
	return (cur - old) / old
}
