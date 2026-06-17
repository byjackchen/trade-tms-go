package indicators

import "math"

// MA is the SEPA simple moving average: a rolling(period).mean() over close.
// Identical to SMA, named for the SEPA call sites (ma(klines, 50) etc.). Output
// is NaN until `period` bars exist.
func MA(close []float64, period int) []float64 { return SMA(close, period) }

// MASlopePct is the SEPA ma_slope_pct:
//
//	if len(klines) < period + lookback: return 0.0
//	series = ma(klines, period)
//	last = series[-1]; prev = series[-1-lookback]
//	if prev == 0 or isnan(prev) or isnan(last): return 0.0
//	return (last - prev) / prev * 100.0
//
// Critically the guard is `< period + lookback` (NOT <=), and the fall-back
// value is 0.0 (not NaN) — used by the stage classifier where 0.0 slope is a
// meaningful "flat" signal. close is the full close history.
func MASlopePct(close []float64, period, lookback int) float64 {
	if len(close) < period+lookback {
		return 0.0
	}
	series := MA(close, period)
	last := series[len(series)-1]
	prev := series[len(series)-1-lookback]
	if prev == 0 || math.IsNaN(prev) || math.IsNaN(last) {
		return 0.0
	}
	return (last - prev) / prev * 100.0
}

// MAUptrendDays is the SEPA ma_uptrend_days:
//
//	if len(klines) < period + 2: return 0
//	series = ma(klines, period).dropna()
//	if series.empty: return 0
//	diffs = series.diff().fillna(0)
//	count = 0
//	for d in reversed(diffs): if d > 0: count += 1 else: break
//	return count
//
// Counts consecutive trailing bars where the MA strictly rose vs the prior bar.
// The .dropna() removes warmup NaNs first, then .diff() (so the first surviving
// diff is 0 from fillna and breaks the streak only if reached). close is the
// full close history.
func MAUptrendDays(close []float64, period int) int {
	if len(close) < period+2 {
		return 0
	}
	full := MA(close, period)
	// dropna: keep only non-NaN MA values, preserving order.
	series := make([]float64, 0, len(full))
	for _, v := range full {
		if !math.IsNaN(v) {
			series = append(series, v)
		}
	}
	if len(series) == 0 {
		return 0
	}
	// diff().fillna(0): diffs[0] = 0, diffs[i] = series[i]-series[i-1].
	diffs := make([]float64, len(series))
	diffs[0] = 0
	for i := 1; i < len(series); i++ {
		diffs[i] = series[i] - series[i-1]
	}
	count := 0
	for i := len(diffs) - 1; i >= 0; i-- {
		if diffs[i] > 0 {
			count++
		} else {
			break
		}
	}
	return count
}

// FractionAbove is the stage classifier's rolling-top fallback:
// (close.tail(n) > ma(...).tail(n)).mean() — the fraction of the last `n` bars
// whose close exceeds the corresponding MA value. Pairs are aligned by position
// over the trailing `n`. NaN MA values count as False (close > NaN is False).
// Returns NaN if n <= 0 or inputs too short.
func FractionAbove(close, ma []float64, n int) float64 {
	if n <= 0 || len(close) < n || len(ma) < n {
		return NaN
	}
	closeTail := close[len(close)-n:]
	maTail := ma[len(ma)-n:]
	count := 0
	for i := 0; i < n; i++ {
		c := closeTail[i]
		m := maTail[i]
		if math.IsNaN(c) || math.IsNaN(m) {
			continue // close > NaN  -> False
		}
		if c > m {
			count++
		}
	}
	return float64(count) / float64(n)
}
