package indicators

import "math"

// RoundHalfEven rounds x to `digits` decimal places using banker's rounding
// (round-half-to-even). Go's math.Round is half-away-from-zero, which diverges
// on exact .5 ties (e.g. RoundHalfEven(2.5)==2 vs math.Round(2.5)==3), so VCP
// depth/score rounding uses this for deterministic, tie-stable output.
//
// Implementation note: the result is the correctly-rounded double nearest to
// the decimal value. We scale, round-half-even the integer part, and unscale;
// for the small digit counts used here (2, 3) this is exact for all practical
// OHLC-derived magnitudes.
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
