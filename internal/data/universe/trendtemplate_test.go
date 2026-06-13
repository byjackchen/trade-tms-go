package universe

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func constSeries(v float64, n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = v
	}
	return out
}

func rampSeries(start, step float64, n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = start + step*float64(i)
	}
	return out
}

// naiveMean is the mathematically obvious reference for the pandas kernel.
func naiveMean(vals []float64) float64 {
	s := 0.0
	for _, v := range vals {
		s += v
	}
	return s / float64(len(vals))
}

func TestPandasRollMeanBasics(t *testing.T) {
	vals := []float64{1, 2, 3, 4, 5}
	out := pandasRollMean(vals, 3)
	require.Len(t, out, 5)
	assert.True(t, math.IsNaN(out[0]), "min_periods == window: NaN before full window")
	assert.True(t, math.IsNaN(out[1]))
	assert.Equal(t, 2.0, out[2])
	assert.Equal(t, 3.0, out[3])
	assert.Equal(t, 4.0, out[4])
}

func TestPandasRollMeanSameValueShortCircuit(t *testing.T) {
	// pandas returns prev_value exactly when the whole window is one value,
	// sidestepping accumulation error.
	out := pandasRollMean(constSeries(0.1, 300), 200)
	assert.Equal(t, 0.1, out[299])
}

func TestPandasRollMeanNaNPoisonsWindow(t *testing.T) {
	vals := rampSeries(1, 1, 10)
	vals[8] = math.NaN()
	out := pandasRollMean(vals, 3)
	assert.True(t, math.IsNaN(out[8]), "window containing NaN -> NaN")
	assert.True(t, math.IsNaN(out[9]))
	assert.False(t, math.IsNaN(out[7]))
}

func TestPandasRollMeanMatchesNaiveWithinTolerance(t *testing.T) {
	// Deterministic pseudo-random walk; the Kahan kernel must agree with a
	// direct mean to ~1 ulp over hundreds of add/remove steps.
	vals := make([]float64, 400)
	x := uint64(88172645463325252)
	price := 100.0
	for i := range vals {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		price += (float64(x%2000) - 1000.0) / 500.0
		vals[i] = price
	}
	out := pandasRollMean(vals, 50)
	for i := 49; i < len(vals); i++ {
		assert.InDelta(t, naiveMean(vals[i-49:i+1]), out[i], 1e-9)
	}
}

func TestRollMaxMinLastAndFallbacks(t *testing.T) {
	vals := rampSeries(1, 1, 252)
	assert.Equal(t, 252.0, rollMaxLast(vals, 252))
	assert.Equal(t, 1.0, rollMinLast(vals, 252))
	assert.True(t, math.IsNaN(rollMaxLast(vals[:251], 252)), "short series -> NaN")

	withNaN := append([]float64(nil), vals...)
	withNaN[100] = math.NaN()
	assert.True(t, math.IsNaN(rollMaxLast(withNaN, 252)), "NaN in window -> NaN")
	assert.Equal(t, 252.0, seriesMax(withNaN), "Series.max skips NaN")
	assert.Equal(t, 1.0, seriesMin(withNaN))
	assert.True(t, math.IsNaN(seriesMax(nil)))
}

func TestMAUptrendDays(t *testing.T) {
	assert.Equal(t, 0, maUptrendDays(rampSeries(1, 1, 201), 200), "needs period+2 bars")
	// Strictly rising closes: every diff positive except the fillna(0)
	// first one -> count = n - period.
	assert.Equal(t, 10, maUptrendDays(rampSeries(1, 1, 210), 200))
	// Flat closes: diffs are 0, no streak.
	assert.Equal(t, 0, maUptrendDays(constSeries(5, 250), 200))
}

func TestEvaluateInsufficientHistory(t *testing.T) {
	res := EvaluateTrendTemplate(nil, nil, nil, 1e9, DefaultMarketCapMinUSD)
	assert.Equal(t, 1, res.PassingRules(), "rule 8 alone with zero bars")
	assert.Equal(t, 0.0, res.Close)
	assert.False(t, res.Passed())

	closes := rampSeries(10, 0.1, 199)
	res = EvaluateTrendTemplate(closes, closes, closes, 0.0, DefaultMarketCapMinUSD)
	assert.Equal(t, 0, res.PassingRules(), "<200 bars and small cap -> 0")
	assert.Equal(t, closes[198], res.Close, "diagnostic close = last close")
	assert.Equal(t, 0.0, res.MA50)
	assert.Equal(t, 0, res.MA200UptrendDays)
}

func TestEvaluateUptrendPassesAll(t *testing.T) {
	// 260 steadily rising bars, large cap: the canonical all-pass setup.
	closes := rampSeries(100, 1, 260)
	highs := rampSeries(101, 1, 260)
	lows := rampSeries(99, 1, 260)
	res := EvaluateTrendTemplate(highs, lows, closes, 1e9, DefaultMarketCapMinUSD)
	assert.True(t, res.Passed(), "rules: %v", res.Rules)
	assert.Equal(t, 8, res.PassingRules())
	assert.Equal(t, 360.0, res.High52w, "rolling 252 high of highs")
	assert.Equal(t, lows[len(lows)-252], res.Low52w)
	assert.Equal(t, 60, res.MA200UptrendDays)
	assert.Equal(t, []string{
		"rule_1_close_gt_ma50", "rule_2_close_gt_ma150", "rule_3_close_gt_ma200",
		"rule_4_ma50_gt_ma150", "rule_5_ma150_gt_ma200",
		"rule_6_within_25pct_of_52w_high", "rule_7_above_30pct_above_52w_low",
		"rule_8_market_cap_above_min",
	}, res.PassingRuleNames())
}

func TestEvaluate52wFallbackUnder252Bars(t *testing.T) {
	// 200..251 bars: rolling 252 is NaN -> full-history max/min fallback.
	n := 210
	closes := rampSeries(100, 1, n)
	highs := rampSeries(101, 1, n)
	lows := rampSeries(99, 1, n)
	res := EvaluateTrendTemplate(highs, lows, closes, 1e9, DefaultMarketCapMinUSD)
	assert.Equal(t, highs[n-1], res.High52w, "fallback = max(high) over full history")
	assert.Equal(t, lows[0], res.Low52w)
}

func TestEvaluateNaNCloseDisablesMARules(t *testing.T) {
	closes := rampSeries(100, 1, 260)
	closes[250] = math.NaN() // poisons every MA window at the last bar
	highs := rampSeries(101, 1, 260)
	lows := rampSeries(99, 1, 260)
	res := EvaluateTrendTemplate(highs, lows, closes, 1e9, DefaultMarketCapMinUSD)
	assert.False(t, res.Rules[0], "NaN MA compares false")
	assert.False(t, res.Rules[1])
	assert.False(t, res.Rules[2])
	assert.False(t, res.Rules[3])
	assert.False(t, res.Rules[4])
	assert.True(t, res.Rules[7], "rule 8 unaffected")
	assert.True(t, math.IsNaN(res.MA50))
}

func TestEvaluateRule7LowGuard(t *testing.T) {
	// low_52w <= 0 -> rule 7 hard false even when close is far above.
	closes := rampSeries(100, 1, 260)
	highs := rampSeries(101, 1, 260)
	lows := rampSeries(99, 1, 260)
	lows[100] = -1 // drags the 252-bar rolling min negative
	res := EvaluateTrendTemplate(highs, lows, closes, 1e9, DefaultMarketCapMinUSD)
	assert.Equal(t, -1.0, res.Low52w)
	assert.False(t, res.Rules[6])
}
