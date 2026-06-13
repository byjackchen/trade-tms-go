package indicators

import "math"

// Trend Template + Stage primitives (SEPA). These pure functions back
// strategies/sepa/trend_template.py and stage.py with the exact constants and
// comparison semantics documented in docs/spec/strategy-sepa.md §3-4
// [MUST-MATCH]. The SEPA strategy builder composes them with VCP + regime.

// Trend Template constants (trend_template.py:37-40).
const (
	TTHighLowWindow = 252  // ~52 weeks
	TTHighTolerance = 0.25 // within 25% of 52w high
	TTLowPremium    = 0.30 // at least 30% above 52w low
	TTMinBars       = 200  // need MA200 history
)

// TrendTemplateResult mirrors the Python dataclass (trend_template.py:43-67):
// the 8 boolean rules plus the diagnostic raw values.
type TrendTemplateResult struct {
	Rule1CloseGtMA50       bool
	Rule2CloseGtMA150      bool
	Rule3CloseGtMA200      bool
	Rule4MA50GtMA150       bool
	Rule5MA150GtMA200      bool
	Rule6Within25PctHigh   bool
	Rule7Above30PctLow     bool
	Rule8MarketCapAboveMin bool

	Close            float64
	MA50             float64
	MA150            float64
	MA200            float64
	High52w          float64
	Low52w           float64
	MarketCapUSD     float64
	MA200UptrendDays int
}

// Passed is true iff all 8 rules pass (trend_template.py:70-80).
func (r TrendTemplateResult) Passed() bool {
	return r.Rule1CloseGtMA50 && r.Rule2CloseGtMA150 && r.Rule3CloseGtMA200 &&
		r.Rule4MA50GtMA150 && r.Rule5MA150GtMA200 && r.Rule6Within25PctHigh &&
		r.Rule7Above30PctLow && r.Rule8MarketCapAboveMin
}

// PassingRules counts how many of the 8 rules pass (trend_template.py:82-96).
func (r TrendTemplateResult) PassingRules() int {
	c := 0
	for _, b := range []bool{
		r.Rule1CloseGtMA50, r.Rule2CloseGtMA150, r.Rule3CloseGtMA200,
		r.Rule4MA50GtMA150, r.Rule5MA150GtMA200, r.Rule6Within25PctHigh,
		r.Rule7Above30PctLow, r.Rule8MarketCapAboveMin,
	} {
		if b {
			c++
		}
	}
	return c
}

// EvaluateTrendTemplate ports trend_template.py evaluate() EXACTLY. close, high,
// low are full-history slices (oldest first). When n < 200, rules 1-7 are False
// and only rule 8 (market cap) is evaluated; diagnostics carry the last close
// (or 0 when n == 0), matching trend_template.py:114-134.
func EvaluateTrendTemplate(close, high, low []float64, marketCapUSD, marketCapMinUSD float64) TrendTemplateResult {
	n := len(close)
	if n < TTMinBars {
		lastClose := 0.0
		if n > 0 {
			lastClose = close[n-1]
		}
		return TrendTemplateResult{
			Rule8MarketCapAboveMin: marketCapUSD >= marketCapMinUSD,
			Close:                  lastClose,
			MarketCapUSD:           marketCapUSD,
		}
	}

	c := close[n-1]
	ma50 := lastNonWindow(MA(close, 50))
	ma150 := lastNonWindow(MA(close, 150))
	ma200 := lastNonWindow(MA(close, 200))

	high52w := FiftyTwoWeekHigh(high, TTHighLowWindow)
	low52w := FiftyTwoWeekLow(low, TTHighLowWindow)

	rule6 := c >= high52w*(1.0-TTHighTolerance)
	rule7 := false
	if low52w > 0 {
		rule7 = c >= low52w*(1.0+TTLowPremium)
	}

	return TrendTemplateResult{
		Rule1CloseGtMA50:       c > ma50,
		Rule2CloseGtMA150:      c > ma150,
		Rule3CloseGtMA200:      c > ma200,
		Rule4MA50GtMA150:       ma50 > ma150,
		Rule5MA150GtMA200:      ma150 > ma200,
		Rule6Within25PctHigh:   rule6,
		Rule7Above30PctLow:     rule7,
		Rule8MarketCapAboveMin: marketCapUSD >= marketCapMinUSD,
		Close:                  c,
		MA50:                   ma50,
		MA150:                  ma150,
		MA200:                  ma200,
		High52w:                high52w,
		Low52w:                 low52w,
		MarketCapUSD:           marketCapUSD,
		MA200UptrendDays:       MAUptrendDays(close, 200),
	}
}

// lastNonWindow returns the last element of a series (the reference reads
// .iloc[-1] of the MA, which at n>=window is a real value).
func lastNonWindow(s []float64) float64 {
	if len(s) == 0 {
		return NaN
	}
	return s[len(s)-1]
}

// Stage classification (stage.py). Constants from stage.py:29-35.
const (
	StageMinBars                = 220
	StageMomentumWindow         = 60
	StageRecentMomentumThresh   = 5.0
	StageSlopeBullThreshold     = 1.0
	StageSlopeBearThreshold     = -1.0
	StageSlopeBaseThreshold     = 0.5
	StageAboveMAHistoryFraction = 0.7
)

// ClassifyStage ports stage.py classify_stage EXACTLY, returning "1"/"2"/"3"/
// "4"/"unknown". close is the full close history (oldest first).
//
// Boundary semantics replicated precisely: the chained strict comparison
// last > ma150 > ma200, the slope/momentum thresholds (strict), the rolling-top
// fallback using FractionAbove over the last 200 bars, and the momentum==5.0
// dead zone where neither Stage 2 nor Stage 3-first fires.
func ClassifyStage(close []float64) string {
	if len(close) < StageMinBars {
		return "unknown"
	}
	ma150 := lastNonWindow(MA(close, 150))
	ma200series := MA(close, 200)
	ma200 := lastNonWindow(ma200series)
	last := close[len(close)-1]
	slope := MASlopePct(close, 200, 20)

	var momentum float64
	if len(close) >= 2*StageMomentumWindow {
		recent := Mean(close[len(close)-StageMomentumWindow:])
		prior := Mean(close[len(close)-2*StageMomentumWindow : len(close)-StageMomentumWindow])
		momentum = (recent - prior) / prior * 100.0
	} else {
		momentum = slope
	}

	// Stage 2: live uptrend.
	if last > ma150 && ma150 > ma200 &&
		slope > StageSlopeBullThreshold &&
		momentum > StageRecentMomentumThresh {
		return "2"
	}
	// Stage 4: downtrend.
	if last < ma150 && ma150 < ma200 && slope < StageSlopeBearThreshold {
		return "4"
	}
	// Stage 3: topping.
	if last > ma200 && slope > StageSlopeBullThreshold && momentum < StageRecentMomentumThresh {
		return "3"
	}
	// Stage 3 fallback: long-term above MA200 with flat slope.
	aboveFrac := FractionAbove(close, ma200series, 200)
	if aboveFrac > StageAboveMAHistoryFraction && math.Abs(slope) <= StageSlopeBullThreshold {
		return "3"
	}
	// Stage 1: base.
	if math.Abs(slope) < StageSlopeBaseThreshold {
		return "1"
	}
	return "unknown"
}
