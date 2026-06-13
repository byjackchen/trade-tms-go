package indicators

import "math"

// FMean is the arithmetic mean over a slice computed as sum/n in float64,
// mirroring Python's statistics.fmean as used by the Pairs OLS/z-score path
// (signal.py:199, 515-516). Empty input returns NaN. Unlike Mean (pandas
// skipna), FMean does NOT skip NaN — it follows Python's fmean which would
// propagate NaN; the Pairs path guarantees finite inputs upstream.
func FMean(x []float64) float64 {
	n := len(x)
	if n == 0 {
		return NaN
	}
	sum := 0.0
	for _, v := range x {
		sum += v
	}
	return sum / float64(n)
}

// PStdev is the POPULATION standard deviation (ddof=0): sqrt(Σ(x-mean)²/N).
// Mirrors Python's statistics.pstdev used by the Pairs z-score
// (signal.py:200). Requires len(x) >= 1; len(x) == 0 returns NaN. A single
// element returns 0.0 (population std of one point).
func PStdev(x []float64) float64 {
	n := len(x)
	if n == 0 {
		return NaN
	}
	mean := FMean(x)
	ss := 0.0
	for _, v := range x {
		d := v - mean
		ss += d * d
	}
	v := ss / float64(n)
	if v < 0 {
		v = 0
	}
	return math.Sqrt(v)
}

// Stdev is the SAMPLE standard deviation (ddof=1): sqrt(Σ(x-mean)²/(N-1)).
// Mirrors Python statistics.stdev / pandas default std. len(x) < 2 returns NaN.
func Stdev(x []float64) float64 {
	n := len(x)
	if n < 2 {
		return NaN
	}
	mean := FMean(x)
	ss := 0.0
	for _, v := range x {
		d := v - mean
		ss += d * d
	}
	v := ss / float64(n-1)
	if v < 0 {
		v = 0
	}
	return math.Sqrt(v)
}

// ZScore returns the population z-score of the last element of `window`
// relative to the whole window: (window[-1] - fmean(window)) / pstdev(window).
// This is exactly the Pairs spread z-score (signal.py:199-203) when `window` is
// the spread series. Returns NaN if len < 2 or pstdev == 0 (the Pairs path
// returns no signal in the std==0 case).
func ZScore(window []float64) float64 {
	if len(window) < 2 {
		return NaN
	}
	mean := FMean(window)
	std := PStdev(window)
	if std == 0 {
		return NaN
	}
	return (window[len(window)-1] - mean) / std
}

// RollingZScore returns a series where each index i (for i >= window-1) holds
// the population z-score of x[i] within the trailing window x[i-window+1..i],
// using rolling mean and rolling population std (ddof=0). Warmup indices are
// NaN; a zero std window yields NaN. This is the general rolling z-score used
// for mean-reversion screens.
func RollingZScore(x []float64, window int) []float64 {
	if window <= 0 {
		panic("indicators: RollingZScore window must be > 0")
	}
	out := make([]float64, len(x))
	for i := range out {
		out[i] = NaN
	}
	if len(x) < window {
		return out
	}
	for i := window - 1; i < len(x); i++ {
		seg := x[i-window+1 : i+1]
		hasNaN := false
		for _, v := range seg {
			if math.IsNaN(v) {
				hasNaN = true
				break
			}
		}
		if hasNaN {
			out[i] = NaN
			continue
		}
		mean := FMean(seg)
		std := PStdev(seg)
		if std == 0 {
			out[i] = NaN
			continue
		}
		out[i] = (x[i] - mean) / std
	}
	return out
}

// OLSResult holds the slope (beta / hedge ratio) and intercept of a simple
// linear regression y = intercept + slope·x.
type OLSResult struct {
	Slope     float64
	Intercept float64
	OK        bool // false when the regression is degenerate (den == 0 or n < 2)
}

// OLSSlope computes only the slope b of y = a + b·x via the closed-form
// covariance/variance ratio, mirroring the Pairs reference _ols_slope
// (signal.py:505-521) EXACTLY:
//
//	n      = len(x); require n == len(y) and n >= 2, else !OK
//	mean_x = fmean(x); mean_y = fmean(y)
//	num    = Σ (x_i - mean_x)(y_i - mean_y)
//	den    = Σ (x_i - mean_x)²
//	slope  = num / den              ; !OK if den == 0
//
// For Pairs, x = short-leg prices, y = long-leg prices, so beta is the
// regression of the long leg ON the short leg.
func OLSSlope(x, y []float64) (float64, bool) {
	n := len(x)
	if n != len(y) || n < 2 {
		return NaN, false
	}
	meanX := FMean(x)
	meanY := FMean(y)
	num := 0.0
	den := 0.0
	for i := 0; i < n; i++ {
		dx := x[i] - meanX
		num += dx * (y[i] - meanY)
		den += dx * dx
	}
	if den == 0 {
		return NaN, false
	}
	return num / den, true
}

// OLS computes both slope and intercept of y = a + b·x. Slope matches
// OLSSlope; intercept = mean_y - slope·mean_x. Degenerate inputs (n < 2 or
// zero x-variance) return OK == false. This is the windowed linear regression
// used for hedge-ratio + intercept; refit by calling it per window.
func OLS(x, y []float64) OLSResult {
	slope, ok := OLSSlope(x, y)
	if !ok {
		return OLSResult{Slope: NaN, Intercept: NaN, OK: false}
	}
	meanX := FMean(x)
	meanY := FMean(y)
	return OLSResult{Slope: slope, Intercept: meanY - slope*meanX, OK: true}
}

// Correlation computes the Pearson correlation coefficient between x and y
// (numpy/pandas corr; population covariance/std cancel the N so ddof is
// irrelevant). Returns NaN when n < 2 or either series has zero variance.
func Correlation(x, y []float64) float64 {
	n := len(x)
	if n != len(y) || n < 2 {
		return NaN
	}
	meanX := FMean(x)
	meanY := FMean(y)
	var sxy, sxx, syy float64
	for i := 0; i < n; i++ {
		dx := x[i] - meanX
		dy := y[i] - meanY
		sxy += dx * dy
		sxx += dx * dx
		syy += dy * dy
	}
	den := math.Sqrt(sxx * syy)
	if den == 0 {
		return NaN
	}
	return sxy / den
}

// Spread computes the per-bar Pairs spread series given long-leg and short-leg
// price slices and a hedge ratio beta: spread_i = long_i - beta·short_i
// (signal.py:196). Panics on length mismatch (the reference uses strict zip).
func Spread(long, short []float64, beta float64) []float64 {
	if len(long) != len(short) {
		panic("indicators: Spread requires equal-length legs")
	}
	out := make([]float64, len(long))
	for i := range long {
		out[i] = long[i] - beta*short[i]
	}
	return out
}
