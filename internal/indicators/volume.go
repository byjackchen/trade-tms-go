package indicators

// VolumeBaselineExcludingCurrent computes the average of the trailing `lookback`
// volume bars EXCLUDING the current (most-recent) bar — the SEPA breakout-volume
// denominator: the mean over volume[-(base+1):-1].
//
// This is the look-ahead guard: including today's bar would inflate the average
// proportional to the breakout's own volume spike, dampening the ratio for
// liquid names. With `base` = lookback:
//
//	requires len(volume) >= base + 1, else returns (NaN, false)
//	baseline = mean(volume[-(base+1) : -1])   # exactly `base` bars, ending the
//	                                           # bar before the last
//
// Returns (baseline, ok). ok is false when there is insufficient history.
func VolumeBaselineExcludingCurrent(volume []float64, base int) (float64, bool) {
	if base < 1 {
		panic("indicators: VolumeBaselineExcludingCurrent base must be >= 1")
	}
	n := len(volume)
	if n < base+1 {
		return NaN, false
	}
	// volume[-(base+1):-1] == volume[n-base-1 : n-1]
	seg := volume[n-base-1 : n-1]
	return Mean(seg), true
}

// BreakoutVolumeOK is the full SEPA breakout-volume gate: with base_lookback
// fixed at 60, today's volume must strictly exceed `multiple` * the
// trailing-`base` baseline (excluding today). Returns false during warmup or
// when the baseline is <= 0 (the `base_avg_vol <= 0` guard).
func BreakoutVolumeOK(volume []float64, base int, multiple float64) bool {
	baseline, ok := VolumeBaselineExcludingCurrent(volume, base)
	if !ok || baseline <= 0 {
		return false
	}
	today := volume[len(volume)-1]
	return today > multiple*baseline
}
