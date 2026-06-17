package indicators

import "math"

// TrueRange computes the per-bar true range series used by ATR.
//
//	TR_i = max( high_i - low_i,
//	            |high_i - close_{i-1}|,
//	            |low_i  - close_{i-1}| )
//
// The first bar has no prior close, so TR_0 = high_0 - low_0 (the standard
// convention; matches ta-lib / pandas_ta). Output length == len(high). high,
// low, close must be equal length (panics otherwise). NaN in any input
// propagates to NaN for that bar.
func TrueRange(high, low, close []float64) []float64 {
	n := len(high)
	if len(low) != n || len(close) != n {
		panic("indicators: TrueRange requires equal-length high/low/close")
	}
	out := make([]float64, n)
	for i := 0; i < n; i++ {
		hl := high[i] - low[i]
		if i == 0 {
			out[i] = hl
			continue
		}
		pc := close[i-1]
		if math.IsNaN(high[i]) || math.IsNaN(low[i]) || math.IsNaN(pc) {
			out[i] = NaN
			continue
		}
		hc := math.Abs(high[i] - pc)
		lc := math.Abs(low[i] - pc)
		tr := hl
		if hc > tr {
			tr = hc
		}
		if lc > tr {
			tr = lc
		}
		out[i] = tr
	}
	return out
}

// ATRWilder computes the Average True Range using Wilder's smoothing (the
// canonical ATR). The seed is the simple average of the first `period` true
// ranges; subsequent values use the recurrence
//
//	ATR_i = (ATR_{i-1} * (period - 1) + TR_i) / period
//
// Output length == len(high). Indices [0, period-1] are NaN (the seed lands at
// index period-1, since TR_0 exists but Wilder needs `period` TRs to seed).
// Specifically out[period-1] = mean(TR[0..period-1]). period <= 0 panics.
func ATRWilder(high, low, close []float64, period int) []float64 {
	if period <= 0 {
		panic("indicators: ATRWilder period must be > 0")
	}
	n := len(high)
	tr := TrueRange(high, low, close)
	out := make([]float64, n)
	for i := range out {
		out[i] = NaN
	}
	if n < period {
		return out
	}
	// Seed: simple mean of the first `period` true ranges.
	seed := 0.0
	for i := 0; i < period; i++ {
		if math.IsNaN(tr[i]) {
			// NaN in the seed window poisons the seed and everything after.
			return out
		}
		seed += tr[i]
	}
	out[period-1] = seed / float64(period)
	for i := period; i < n; i++ {
		prev := out[i-1]
		if math.IsNaN(prev) || math.IsNaN(tr[i]) {
			out[i] = NaN
			continue
		}
		out[i] = (prev*float64(period-1) + tr[i]) / float64(period)
	}
	return out
}

// ATRSimple computes ATR as a plain simple moving average of the true range
// (a rolling(period).mean() over TrueRange). Some strategies use this instead
// of Wilder smoothing; provided for completeness and flexibility. Warmup
// indices [0, period-1] are NaN.
func ATRSimple(high, low, close []float64, period int) []float64 {
	if period <= 0 {
		panic("indicators: ATRSimple period must be > 0")
	}
	tr := TrueRange(high, low, close)
	return SMA(tr, period)
}
