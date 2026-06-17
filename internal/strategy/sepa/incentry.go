package sepa

// incentry.go is the incremental hot-path replacement for the batch
// ClassifyStage / EvaluateTrendTemplate calls in maybeEnter. Each function here
// reproduces the corresponding internal/indicators batch routine BYTE-FOR-BYTE
// (same constants, same strict comparisons, same NaN/fallback semantics) but
// consumes the per-generator incState (fresh-sum MAs + the maintained MA200
// series + the 252 high/low accumulators) instead of recomputing O(n*window)
// batch indicators every bar. See incstate.go for the batch-agreement contract.
//
// BATCH AGREEMENT: confirmed byte-for-byte against the batch source —
//   - indicators.ClassifyStage (trend_template.go:141)
//   - indicators.EvaluateTrendTemplate (trend_template.go:66)
//   - indicators.MASlopePct / FractionAbove (ma.go)
// The fixtures keep the SEPA golden test at 0 mismatches.

import (
	"math"

	"github.com/byjackchen/trade-tms-go/internal/indicators"
)

// classifyStageInc matches indicators.ClassifyStage(g.close) exactly using the
// incremental state. Returns "1"/"2"/"3"/"4"/"unknown".
func (g *Generator) classifyStageInc() string {
	close := g.close
	n := len(close)
	if n < indicators.StageMinBars {
		return "unknown"
	}

	ma150 := freshSMA(close, 150)            // == lastNonWindow(MA(close,150))
	ma200 := g.inc.ma200[len(g.inc.ma200)-1] // == lastNonWindow(MA(close,200))
	last := close[n-1]
	slope := g.maSlopePct200x20() // == MASlopePct(close,200,20)

	var momentum float64
	if n >= 2*indicators.StageMomentumWindow {
		recent := meanTail(close, indicators.StageMomentumWindow, 0)
		prior := meanTail(close, indicators.StageMomentumWindow, indicators.StageMomentumWindow)
		momentum = (recent - prior) / prior * 100.0
	} else {
		momentum = slope
	}

	// Stage 2: live uptrend.
	if last > ma150 && ma150 > ma200 &&
		slope > indicators.StageSlopeBullThreshold &&
		momentum > indicators.StageRecentMomentumThresh {
		return "2"
	}
	// Stage 4: downtrend.
	if last < ma150 && ma150 < ma200 && slope < indicators.StageSlopeBearThreshold {
		return "4"
	}
	// Stage 3: topping.
	if last > ma200 && slope > indicators.StageSlopeBullThreshold && momentum < indicators.StageRecentMomentumThresh {
		return "3"
	}
	// Stage 3 fallback: long-term above MA200 with flat slope.
	aboveFrac := g.fractionAbove200()
	if aboveFrac > indicators.StageAboveMAHistoryFraction && math.Abs(slope) <= indicators.StageSlopeBullThreshold {
		return "3"
	}
	// Stage 1: base.
	if math.Abs(slope) < indicators.StageSlopeBaseThreshold {
		return "1"
	}
	return "unknown"
}

// maSlopePct200x20 matches indicators.MASlopePct(g.close, 200, 20) using the
// maintained MA200 series: last = ma200[-1]; prev = ma200[-1-20]; guard
// (prev==0||NaN||last NaN) -> 0.0; else (last-prev)/prev*100.
func (g *Generator) maSlopePct200x20() float64 {
	const period, lookback = 200, 20
	if len(g.close) < period+lookback {
		return 0.0
	}
	ma := g.inc.ma200
	last := ma[len(ma)-1]
	prev := ma[len(ma)-1-lookback]
	if prev == 0 || math.IsNaN(prev) || math.IsNaN(last) {
		return 0.0
	}
	return (last - prev) / prev * 100.0
}

// fractionAbove200 matches indicators.FractionAbove(g.close, MA(close,200), 200)
// over the maintained MA200 series: fraction of the last 200 bars whose close
// exceeds the aligned MA200 value (NaN MA counts as False).
//
// BATCH AGREEMENT across trim: the batch recomputes MA(g.close,200) over the CURRENT
// (trimmed) buffer, so MA200 is NaN for the first 200-1==199 buffer indices
// (warmup). The maintained ma200 series carries pre-trim real values at those
// positions, so we must mask any buffer index < 199 back to NaN to match batch.
// When len(close) >= 399 the trailing-200 window never reaches the warmup region
// and the mask is a no-op (the production cap is 1000), but it is required for
// shorter HistoryMaxBars.
func (g *Generator) fractionAbove200() float64 {
	const n = 200
	close := g.close
	ma := g.inc.ma200
	bufLen := len(close)
	if bufLen < n || len(ma) < n {
		return indicators.NaN
	}
	start := bufLen - n // absolute buffer index of the first compared bar
	closeTail := close[start:]
	maTail := ma[len(ma)-n:]
	count := 0
	for i := 0; i < n; i++ {
		c := closeTail[i]
		m := maTail[i]
		// Batch MA200 is NaN (warmup) for buffer indices < 199.
		if start+i < n-1 {
			continue
		}
		if math.IsNaN(c) || math.IsNaN(m) {
			continue
		}
		if c > m {
			count++
		}
	}
	return float64(count) / float64(n)
}

// trendTemplatePassInc matches indicators.EvaluateTrendTemplate(...).Passed()
// for the entry path (n >= 200 guaranteed by maybeEnter's n<200 guard). Only the
// 8 boolean rules are needed; MA200UptrendDays (a diagnostic) is not read in the
// entry path so it is not computed. Byte-identical rule evaluation.
func (g *Generator) trendTemplatePassInc() bool {
	close := g.close
	n := len(close)
	// maybeEnter already returns early when n < 200, but keep the batch guard so
	// rule semantics are identical if ever called with short history.
	if n < indicators.TTMinBars {
		return g.marketCapUSD >= g.cfg.MarketCapMinUSD &&
			false // rules 1-7 False -> Passed() False unless all true (never here)
	}

	c := close[n-1]
	ma50 := freshSMA(close, 50)
	ma150 := freshSMA(close, 150)
	ma200 := g.inc.ma200[len(g.inc.ma200)-1]

	high52w := g.inc.fiftyTwoWeekHigh()
	low52w := g.inc.fiftyTwoWeekLow()

	rule1 := c > ma50
	rule2 := c > ma150
	rule3 := c > ma200
	rule4 := ma50 > ma150
	rule5 := ma150 > ma200
	rule6 := c >= high52w*(1.0-indicators.TTHighTolerance)
	rule7 := false
	if low52w > 0 {
		rule7 = c >= low52w*(1.0+indicators.TTLowPremium)
	}
	rule8 := g.marketCapUSD >= g.cfg.MarketCapMinUSD

	return rule1 && rule2 && rule3 && rule4 && rule5 && rule6 && rule7 && rule8
}

// meanTail returns Mean(close[len-off-w : len-off]) skipping NaN. off is the
// count of trailing bars to exclude; w is the window size. The SEPA momentum
// windows are NaN-free so this matches indicators.Mean over the same slice
// exactly.
func meanTail(close []float64, w, off int) float64 {
	n := len(close)
	hi := n - off
	lo := hi - w
	sum := 0.0
	cnt := 0
	for j := lo; j < hi; j++ {
		v := close[j]
		if math.IsNaN(v) {
			continue
		}
		sum += v
		cnt++
	}
	if cnt == 0 {
		return indicators.NaN
	}
	return sum / float64(cnt)
}
