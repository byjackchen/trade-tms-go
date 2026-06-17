package metrics

// metrics.go defines the backtest performance metrics (spec §1). The formulas,
// defaults and edge cases are the canonical definitions for this library.
//
// The mean and population standard deviation are computed with exact rational
// arithmetic (math/big) so they are bit-for-bit reproducible across platforms
// (arm64 vs x86), independent of compiler instruction selection (spec §1.3).
// Each float is converted to an exact rational, accumulated exactly, and
// converted to float64 only once — so a curve whose per-period returns are
// bit-identical (e.g. a constant compounding rate, returns [0.1,0.1,0.1])
// yields mean==0.1 exactly and pstdev==0.0 exactly, which makes the vol==0
// guard in Sharpe fire and return 0.0. A float64 compensated sum (Neumaier) is
// NOT sufficient: neumaierMean([0.1,0.1,0.1]) is 0.10000000000000002 (1 ulp
// high), so pstdev becomes ~1.4e-17 (not 0), the guard never fires and Sharpe
// explodes to ~1.1e17. Exact rational mean/pstdev makes the result determinate
// (verified over 200k random vectors).

import (
	"math"
	"math/big"
)

// PeriodsPerYear is the annualization base used by sharpe and calmar
// (spec §1.3/§1.5). No call site overrides it.
const PeriodsPerYear = 252

// CalmarZeroDDDivisor is the synthetic 1% drawdown floor applied to a
// zero-drawdown positive-growth curve (spec §1.5).
const CalmarZeroDDDivisor = 0.01

// BacktestMetrics is the field set and JSON key names of the backtest metrics
// (spec §1.1). It is serialized verbatim into trial_*.json `metrics` and
// run_metrics rows.
type BacktestMetrics struct {
	FinalBalanceUSD   float64 `json:"final_balance_usd"`
	TotalPnLUSD       float64 `json:"total_pnl_usd"`
	Sharpe            float64 `json:"sharpe"`
	Calmar            float64 `json:"calmar"`
	MaxDrawdownPct    float64 `json:"max_drawdown_pct"`
	NumOrders         int     `json:"num_orders"`
	NumFilledOrders   int     `json:"num_filled_orders"`
	NumRejectedOrders int     `json:"num_rejected_orders"`
	NumPositions      int     `json:"num_positions"`
}

// Objectives returns the (sharpe, calmar) tuple reported to the optimizer —
// the objective ordering used by the optimizer (spec §1.1).
func (m BacktestMetrics) Objectives() (float64, float64) { return m.Sharpe, m.Calmar }

// Returns computes per-period simple returns over consecutive pairs of the
// curve (spec §1.2). A zero previous value DROPS that pair entirely (the
// returns count shrinks); it never emits a 0 return.
func Returns(curve []float64) []float64 {
	if len(curve) < 2 {
		return nil
	}
	out := make([]float64, 0, len(curve)-1)
	for i := 1; i < len(curve); i++ {
		prev, cur := curve[i-1], curve[i]
		if prev == 0 {
			continue
		}
		out = append(out, (cur-prev)/prev)
	}
	return out
}

// Sharpe is the Sharpe ratio (spec §1.3):
//
//	r = returns(curve)
//	if len(r) < 2: return 0
//	vol = pstdev(r)            # population std-dev, ddof = 0
//	if vol == 0: return 0
//	return mean(r) / vol * sqrt(252)
//
// Risk-free rate is none (raw mean). Flat curves and curves with fewer than
// two returns yield 0.0.
func Sharpe(curve []float64) float64 {
	r := Returns(curve)
	if len(r) < 2 {
		return 0.0
	}
	m := exactMean(r)
	vol := exactPstdev(r)
	if vol == 0 {
		return 0.0
	}
	return m / vol * math.Sqrt(float64(PeriodsPerYear))
}

// MaxDrawdownPct is the maximum drawdown percent (spec §1.4):
// returns a NON-POSITIVE percent. Empty curve and monotonic-up curve -> 0.0;
// a zero peak is skipped (no division by zero).
func MaxDrawdownPct(curve []float64) float64 {
	if len(curve) == 0 {
		return 0.0
	}
	peak := curve[0]
	maxDD := 0.0
	for _, v := range curve {
		if v > peak {
			peak = v
		}
		if peak == 0 {
			continue
		}
		dd := (v - peak) / peak * 100.0
		if dd < maxDD {
			maxDD = dd
		}
	}
	return maxDD
}

// Calmar is the Calmar ratio (spec §1.5), including all special cases:
//
//	if len(curve) < 2 or curve[0] == 0: return 0
//	total_return = curve[-1]/curve[0] - 1
//	if total_return <= -1: return -1
//	years = max((len(curve)-1)/252, 1/252)
//	ann = (1+total_return)^(1/years) - 1
//	mdd = abs(max_drawdown_pct(curve)) / 100
//	if mdd == 0: return 0 if ann <= 0 else ann/0.01
//	return ann / mdd
func Calmar(curve []float64) float64 {
	if len(curve) < 2 || curve[0] == 0 {
		return 0.0
	}
	totalReturn := curve[len(curve)-1]/curve[0] - 1.0
	if totalReturn <= -1.0 {
		return -1.0
	}
	years := float64(len(curve)-1) / float64(PeriodsPerYear)
	if floor := 1.0 / float64(PeriodsPerYear); years < floor {
		years = floor
	}
	ann := math.Pow(1.0+totalReturn, 1.0/years) - 1.0
	mdd := math.Abs(MaxDrawdownPct(curve)) / 100.0
	if mdd == 0 {
		if ann <= 0 {
			return 0.0
		}
		return ann / CalmarZeroDDDivisor
	}
	return ann / mdd
}

// TotalReturn is curve[-1]/curve[0] - 1 (the same quantity calmar derives), or
// 0 when the curve is too short or starts at zero. Exposed for reporting; not a
// core metric field.
func TotalReturn(curve []float64) float64 {
	if len(curve) < 2 || curve[0] == 0 {
		return 0.0
	}
	return curve[len(curve)-1]/curve[0] - 1.0
}

// Volatility is the population standard deviation of the per-period returns
// (the denominator of Sharpe before annualization). 0 for fewer than two
// returns or a flat curve. Exposed for reporting; not a core metric field.
func Volatility(curve []float64) float64 {
	r := Returns(curve)
	if len(r) < 2 {
		return 0.0
	}
	return exactPstdev(r)
}

// Compute assembles the curve-derived metrics (sharpe, calmar,
// max_drawdown_pct) plus the supplied balances and counters into a
// BacktestMetrics. final/starting balances set final_balance_usd and
// total_pnl_usd = final - starting (spec §1.1).
func Compute(curve []float64, startingBalance, finalBalance float64, counts Counts) BacktestMetrics {
	return BacktestMetrics{
		FinalBalanceUSD:   finalBalance,
		TotalPnLUSD:       finalBalance - startingBalance,
		Sharpe:            Sharpe(curve),
		Calmar:            Calmar(curve),
		MaxDrawdownPct:    MaxDrawdownPct(curve),
		NumOrders:         counts.NumOrders,
		NumFilledOrders:   counts.NumFilledOrders,
		NumRejectedOrders: counts.NumRejectedOrders,
		NumPositions:      counts.NumPositions,
	}
}

// Counts carries the four order/position counters the equity curve cannot
// supply (spec §1.6).
type Counts struct {
	NumOrders         int
	NumFilledOrders   int
	NumRejectedOrders int
	NumPositions      int
}

// ---------------------------------------------------------------------------
// Exact-rational mean / population std-dev (spec §1.3).
//
// These are computed in exact rational arithmetic so the result is bit-for-bit
// reproducible across platforms (arm64 vs x86), independent of compiler
// instruction selection. Each float64 is converted to an EXACT rational
// (big.Rat.SetFloat64); accumulation is exact; the final float64 conversion is
// correctly rounded (big.Rat.Float64). The std-dev uses an exact sum of
// squared deviations followed by a correctly-rounded rational sqrt:
//
//	sx  = Σ rat(x_i)
//	sxx = Σ rat(x_i)^2
//	ssd = (n*sxx - sx*sx) / n                 # exact Σ(x_i-mean)^2
//	pvar = ssd / n
//	pstdev = floatSqrtOfFrac(pvar.num, pvar.den)   # correctly-rounded sqrt
//
// The crucial property: pvar == 0 (rational) IFF all deviations are exactly 0,
// so the vol==0 guard in Sharpe fires deterministically.
// ---------------------------------------------------------------------------

// ratStats returns (sx, ssd) as exact rationals: sx = Σ rat(x_i) and
// ssd = (n*Σx_i² − (Σx_i)²)/n, the exact sum of squared deviations from the
// exact mean. Callers guarantee len>0.
func ratStats(xs []float64) (sx, ssd *big.Rat) {
	sx = new(big.Rat)
	sxx := new(big.Rat)
	tmp := new(big.Rat)
	sq := new(big.Rat)
	for _, x := range xs {
		// big.Rat.SetFloat64 is the exact value of the float64.
		tmp.SetFloat64(x)
		sx.Add(sx, tmp)
		sq.Mul(tmp, tmp)
		sxx.Add(sxx, sq)
	}
	n := big.NewRat(int64(len(xs)), 1)
	// ssd = (n*sxx - sx*sx) / n
	nSxx := new(big.Rat).Mul(n, sxx)
	sxSq := new(big.Rat).Mul(sx, sx)
	ssd = new(big.Rat).Sub(nSxx, sxSq)
	ssd.Quo(ssd, n)
	return sx, ssd
}

// exactMean returns float64(Σ rat(x_i) / n), the exact arithmetic mean.
// Callers guarantee len(xs) > 0.
func exactMean(xs []float64) float64 {
	sx, _ := ratStats(xs)
	mean := new(big.Rat).Quo(sx, big.NewRat(int64(len(xs)), 1))
	f, _ := mean.Float64()
	return f
}

// exactPstdev returns the population standard deviation (ddof=0), computed
// exactly for cross-platform reproducibility. Callers guarantee len(xs) > 0.
func exactPstdev(xs []float64) float64 {
	_, ssd := ratStats(xs)
	pvar := new(big.Rat).Quo(ssd, big.NewRat(int64(len(xs)), 1))
	return floatSqrtOfFrac(pvar.Num(), pvar.Denom())
}

// sqrtBitWidth (109) is the rounding precision for the correctly-rounded
// rational sqrt.
const sqrtBitWidth = 109

// floatSqrtOfFrac returns the correctly-rounded float64 square root of n/m
// (m > 0, n >= 0), via an exact integer sqrt with round-to-odd. Computing the
// sqrt this way keeps the result determinate across platforms.
func floatSqrtOfFrac(n, m *big.Int) float64 {
	if n.Sign() == 0 {
		return 0.0
	}
	// q = (n.bit_length() - m.bit_length() - sqrtBitWidth) // 2  (floor div)
	q := floorDiv(n.BitLen()-m.BitLen()-sqrtBitWidth, 2)
	var numerator *big.Int
	denominator := big.NewInt(1)
	if q >= 0 {
		// numerator = isqrt_rto(n, m << 2q) << q
		shifted := new(big.Int).Lsh(m, uint(2*q))
		numerator = integerSqrtOfFracRTO(n, shifted)
		numerator.Lsh(numerator, uint(q))
	} else {
		// numerator = isqrt_rto(n << -2q, m); denominator = 1 << -q
		shiftedN := new(big.Int).Lsh(n, uint(-2*q))
		numerator = integerSqrtOfFracRTO(shiftedN, m)
		denominator.Lsh(denominator, uint(-q))
	}
	// numerator / denominator converted to float64, correctly rounded.
	res := new(big.Rat).SetFrac(numerator, denominator)
	f, _ := res.Float64()
	return f
}

// integerSqrtOfFracRTO returns isqrt(n/m) rounded to the nearest integer using
// round-to-odd:
//
//	a = isqrt(n // m); return a | (a*a*m != n)
func integerSqrtOfFracRTO(n, m *big.Int) *big.Int {
	q := new(big.Int).Quo(n, m) // n // m (both non-negative)
	a := new(big.Int).Sqrt(q)
	// odd-bit if a*a*m != n
	aam := new(big.Int).Mul(a, a)
	aam.Mul(aam, m)
	if aam.Cmp(n) != 0 {
		a.Or(a, big.NewInt(1))
	}
	return a
}

// floorDiv returns floor division a // b for b > 0.
func floorDiv(a, b int) int {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}
