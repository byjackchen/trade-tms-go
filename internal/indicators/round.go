package indicators

import "math"

// RoundHalfEven rounds x to `digits` decimal places using banker's rounding
// (round-half-to-even), matching Python's built-in round(). Go's math.Round is
// half-away-from-zero, which diverges on exact .5 ties (e.g. round(2.5)==2 in
// Python vs 3 in Go), so VCP depth/score rounding must use this to stay
// bit-for-bit with the reference (vcp.py round(x,2)/round(x,3)).
//
// Implementation note: like CPython, the result is the correctly-rounded double
// nearest to the decimal value. We scale, round-half-even the integer part, and
// unscale; for the small digit counts used here (2, 3) this matches CPython's
// _Py_dg_dtoa-based round for all practical OHLC-derived magnitudes.
func RoundHalfEven(x float64, digits int) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return x
	}
	pow := math.Pow(10, float64(digits))
	scaled := x * pow
	floor := math.Floor(scaled)
	diff := scaled - floor
	var rounded float64
	switch {
	case diff < 0.5:
		rounded = floor
	case diff > 0.5:
		rounded = floor + 1
	default:
		// exact .5 tie -> round to even
		if math.Mod(floor, 2) == 0 {
			rounded = floor
		} else {
			rounded = floor + 1
		}
	}
	return rounded / pow
}
