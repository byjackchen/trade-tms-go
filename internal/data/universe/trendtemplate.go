package universe

// trendtemplate.go ports strategies/sepa/trend_template.py and the pandas
// rolling primitives it uses (strategies/sepa/_indicators.py) with
// bit-equivalent float64 arithmetic. See docs/spec/calendar-universe.md §3.5.
//
// The moving averages replicate pandas 2.x `Series.rolling(w).mean()`
// (pandas/_libs/window/aggregations.pyx roll_mean): an online add/remove
// pass over the whole series with Kahan compensation, same-value short
// circuit and sign-count clamps. A naive per-window sum can differ in the
// last ulp, which the strict `>` rule comparisons must not see.

import "math"

// Trend-template constants.
const (
	// DefaultMarketCapMinUSD is rule 8's default threshold.
	DefaultMarketCapMinUSD = 500_000_000.0
	// highLowWindow is the 52-week high/low window in trading bars.
	highLowWindow = 252
	// highTolerance: rule 6 passes when close >= high52w * (1 - 0.25).
	highTolerance = 0.25
	// lowPremium: rule 7 passes when close >= low52w * (1 + 0.30).
	lowPremium = 0.30
)

// RuleNames are the field names of the 8 rules, reused as snapshot member
// "reasons" (rank order = rule order).
var RuleNames = [8]string{
	"rule_1_close_gt_ma50",
	"rule_2_close_gt_ma150",
	"rule_3_close_gt_ma200",
	"rule_4_ma50_gt_ma150",
	"rule_5_ma150_gt_ma200",
	"rule_6_within_25pct_of_52w_high",
	"rule_7_above_30pct_above_52w_low",
	"rule_8_market_cap_above_min",
}

// TrendTemplateResult mirrors trend_template.TrendTemplateResult: per-rule
// flags plus the diagnostic values the rules were computed from.
type TrendTemplateResult struct {
	Rules [8]bool

	Close            float64
	MA50             float64
	MA150            float64
	MA200            float64
	High52w          float64
	Low52w           float64
	MarketCapUSD     float64
	MA200UptrendDays int
}

// Passed reports whether all 8 rules pass.
func (r TrendTemplateResult) Passed() bool {
	for _, ok := range r.Rules {
		if !ok {
			return false
		}
	}
	return true
}

// PassingRules counts the passing rules (0-8); this is the screener score
// input.
func (r TrendTemplateResult) PassingRules() int {
	n := 0
	for _, ok := range r.Rules {
		if ok {
			n++
		}
	}
	return n
}

// PassingRuleNames returns the names of the passing rules in rule order.
func (r TrendTemplateResult) PassingRuleNames() []string {
	out := make([]string, 0, 8)
	for i, ok := range r.Rules {
		if ok {
			out = append(out, RuleNames[i])
		}
	}
	return out
}

// EvaluateTrendTemplate evaluates the 8 Minervini criteria over parallel
// high/low/close series (oldest first) (spec §3.5):
//
//   - n < 200 bars: rules 1-7 false, rule 8 still evaluated; diagnostics
//     zeroed except Close (last close, or 0 when n == 0).
//   - n >= 200: MAs are simple rolling means; the 52-week levels use the
//     252-bar rolling max/min (NaN until a full clean window exists) with
//     a full-history skip-NaN fallback.
//   - All comparisons are plain float64; NaN compares false.
func EvaluateTrendTemplate(highs, lows, closes []float64, marketCapUSD, marketCapMinUSD float64) TrendTemplateResult {
	n := len(closes)
	if n < 200 {
		lastClose := 0.0
		if n > 0 {
			lastClose = closes[n-1]
		}
		return TrendTemplateResult{
			Rules:        [8]bool{7: marketCapUSD >= marketCapMinUSD},
			Close:        lastClose,
			MarketCapUSD: marketCapUSD,
		}
	}

	closeV := closes[n-1]
	ma50 := pandasRollMeanLast(closes, 50)
	ma150 := pandasRollMeanLast(closes, 150)
	ma200 := pandasRollMeanLast(closes, 200)

	high52w := rollMaxLast(highs, highLowWindow)
	low52w := rollMinLast(lows, highLowWindow)
	// rolling min_periods == window: NaN until a full clean window exists;
	// fall back to the expanding (full-history, skip-NaN) range.
	if math.IsNaN(high52w) {
		high52w = seriesMax(highs)
	}
	if math.IsNaN(low52w) {
		low52w = seriesMin(lows)
	}

	rule6 := closeV >= high52w*(1.0-highTolerance)
	rule7 := false
	if low52w > 0 {
		rule7 = closeV >= low52w*(1.0+lowPremium)
	}

	return TrendTemplateResult{
		Rules: [8]bool{
			closeV > ma50,
			closeV > ma150,
			closeV > ma200,
			ma50 > ma150,
			ma150 > ma200,
			rule6,
			rule7,
			marketCapUSD >= marketCapMinUSD,
		},
		Close:            closeV,
		MA50:             ma50,
		MA150:            ma150,
		MA200:            ma200,
		High52w:          high52w,
		Low52w:           low52w,
		MarketCapUSD:     marketCapUSD,
		MA200UptrendDays: maUptrendDays(closes, 200),
	}
}

// ---------------------------------------------------------------------------
// pandas rolling emulation
// ---------------------------------------------------------------------------

// pandasRollMean replicates pandas 2.x fixed-window roll_mean with
// min_periods == window: out[i] is NaN until the window is full and
// whenever the window contains NaN. The accumulator state (Kahan
// compensation for adds and removes, consecutive-same-value tracking,
// negative-count clamps) is carried across the whole series exactly like
// the Cython kernel, so the emitted floats are bit-identical to pandas.
func pandasRollMean(vals []float64, window int) []float64 {
	n := len(vals)
	out := make([]float64, n)
	if window < 1 {
		window = 1
	}

	var (
		sum, compAdd, compRemove float64
		nobs, negCt              int64
		prevValue                float64
		numSameValue             int64
	)

	addMean := func(val float64) {
		if math.IsNaN(val) {
			return
		}
		nobs++
		y := val - compAdd
		t := sum + y
		compAdd = t - sum - y
		sum = t
		if math.Signbit(val) {
			negCt++
		}
		if val == prevValue {
			numSameValue++
		} else {
			numSameValue = 1
		}
		prevValue = val
	}
	removeMean := func(val float64) {
		if math.IsNaN(val) {
			return
		}
		nobs--
		y := -val - compRemove
		t := sum + y
		compRemove = t - sum - y
		sum = t
		if math.Signbit(val) {
			negCt--
		}
	}
	calcMean := func() float64 {
		if nobs >= int64(window) && nobs > 0 {
			result := sum / float64(nobs)
			switch {
			case numSameValue >= nobs:
				result = prevValue
			case negCt == 0 && result < 0:
				result = 0
			case negCt == nobs && result > 0:
				result = 0
			}
			return result
		}
		return math.NaN()
	}

	prevStart, prevEnd := 0, 0
	for i := 0; i < n; i++ {
		s := i + 1 - window
		if s < 0 {
			s = 0
		}
		e := i + 1
		if i == 0 || s >= prevEnd {
			// Fresh (or non-overlapping) window: full state reset, exactly
			// like the Cython kernel's first branch.
			sum, compAdd, compRemove = 0, 0, 0
			nobs, negCt = 0, 0
			prevValue = vals[s]
			numSameValue = 0
			for j := s; j < e; j++ {
				addMean(vals[j])
			}
		} else {
			for j := prevStart; j < s; j++ {
				removeMean(vals[j])
			}
			for j := prevEnd; j < e; j++ {
				addMean(vals[j])
			}
		}
		prevStart, prevEnd = s, e
		out[i] = calcMean()
	}
	return out
}

// pandasRollMeanLast returns the last value of pandasRollMean.
func pandasRollMeanLast(vals []float64, window int) float64 {
	out := pandasRollMean(vals, window)
	if len(out) == 0 {
		return math.NaN()
	}
	return out[len(out)-1]
}

// rollMaxLast is the last value of Series.rolling(window).max() with
// min_periods == window: NaN when fewer than `window` observations exist
// or the trailing window contains NaN; otherwise the window max (an exact
// comparison-only reduction — no accumulation error to emulate).
func rollMaxLast(vals []float64, window int) float64 {
	n := len(vals)
	if n < window {
		return math.NaN()
	}
	cur := math.NaN()
	for _, v := range vals[n-window:] {
		if math.IsNaN(v) {
			return math.NaN()
		}
		if math.IsNaN(cur) || v > cur {
			cur = v
		}
	}
	return cur
}

// rollMinLast mirrors rollMaxLast for the rolling minimum.
func rollMinLast(vals []float64, window int) float64 {
	n := len(vals)
	if n < window {
		return math.NaN()
	}
	cur := math.NaN()
	for _, v := range vals[n-window:] {
		if math.IsNaN(v) {
			return math.NaN()
		}
		if math.IsNaN(cur) || v < cur {
			cur = v
		}
	}
	return cur
}

// seriesMax is pandas Series.max(): skip NaN; NaN when empty or all-NaN.
func seriesMax(vals []float64) float64 {
	cur := math.NaN()
	for _, v := range vals {
		if math.IsNaN(v) {
			continue
		}
		if math.IsNaN(cur) || v > cur {
			cur = v
		}
	}
	return cur
}

// seriesMin is pandas Series.min(): skip NaN; NaN when empty or all-NaN.
func seriesMin(vals []float64) float64 {
	cur := math.NaN()
	for _, v := range vals {
		if math.IsNaN(v) {
			continue
		}
		if math.IsNaN(cur) || v < cur {
			cur = v
		}
	}
	return cur
}

// maUptrendDays ports _indicators.ma_uptrend_days (diagnostic only,
// spec §3.5): count of consecutive trailing positive first differences of
// the MA(period) series; the first difference is fill(0) and breaks any
// streak that reaches it.
func maUptrendDays(closes []float64, period int) int {
	if len(closes) < period+2 {
		return 0
	}
	series := pandasRollMean(closes, period)
	kept := make([]float64, 0, len(series))
	for _, v := range series {
		if !math.IsNaN(v) {
			kept = append(kept, v)
		}
	}
	if len(kept) == 0 {
		return 0
	}
	diffs := make([]float64, len(kept))
	diffs[0] = 0 // diff() yields NaN first; fillna(0)
	for i := 1; i < len(kept); i++ {
		diffs[i] = kept[i] - kept[i-1]
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
