package sepa

// intent.go computes evaluate_signal and state_summary — the read-only UI
// projections. evaluate_signal's only state mutation is the monotonic
// generation increment (the counter advances on every call).
//
// Deliberate asymmetry (spec §12 #7): evaluate_signal's VCP runs on the FULL
// klines (INCLUDING the current bar), unlike the entry path which excludes it.
// This is a UI-only path, not a trading decision; do not "fix" it.

import (
	"time"

	"github.com/byjackchen/trade-tms-go/internal/indicators"
)

// EvaluateSignal returns a typed UI snapshot of the current setup state.
// asOf is the snapshot timestamp (the bar ts in the
// streaming path). It increments the generation counter on every call.
func (g *Generator) EvaluateSignal(asOf time.Time) SignalSnapshot {
	g.intentGeneration++

	pivotStr := ""
	if g.pivotPrice.val > 0 {
		pivotStr = g.pivotPrice.str
	}
	stopStr := ""
	if g.stopPrice.val > 0 {
		stopStr = g.stopPrice.str
	}

	base := SignalSnapshot{
		Symbol:     g.cfg.Symbol,
		UpdatedAt:  asOf,
		Generation: g.intentGeneration,
		StrategyID: StrategyID,
		PivotPrice: pivotStr,
		StopPrice:  stopStr,
	}

	n := len(g.close)

	// 1. Empty or < 50 bars -> NO_SETUP.
	if n < 50 {
		base.State = StateNoSetup
		base.Strength = 0.0
		base.TrendTemplatePass = false
		base.Grade = 0
		return base
	}

	lastClose := g.close[n-1] // Decimal(str(close.iloc[-1]))

	// 2. Held + stop>0 + last_close < stop (STRICT <) -> STOP_HIT.
	//    Note: on_bar uses <=; intent uses < (the documented predicate mismatch,
	//    spec §8.8 [IMPROVE]).
	if g.position != 0 && g.stopPrice.val > 0 && lastClose < g.stopPrice.val {
		base.State = StateStopHit
		base.Strength = 0.0
		base.TrendTemplatePass = false
		base.Grade = 0
		return base
	}

	// 3. Held normally -> HOLD. tt_pass hard-coded true.
	if g.position != 0 {
		base.State = StateHold
		base.Strength = 50.0
		base.TrendTemplatePass = true
		base.Grade = 0
		// A held position has a real entry pivot + stop; attach the trade-plan
		// metrics (signed proximity vs the entry pivot, risk vs the live stop,
		// %off-52wk-high, vol_ratio) so the watchlist row stays actionable for
		// the management decision too.
		g.attachHeldTradePlan(&base, lastClose)
		return base
	}

	// 4. Flat: classify with trend template + VCP (FULL klines).
	tt := indicators.EvaluateTrendTemplate(
		g.close, g.high, g.low, g.marketCapUSD, g.cfg.MarketCapMinUSD,
	)
	ttGrade := int(float64(tt.PassingRules()) / 8 * 100) // int() truncation

	var vcp indicators.VCPSnapshot
	haveVCP := false
	if n >= 30 {
		vcp, haveVCP = indicators.DetectVCP(
			g.high, g.low, g.volume,
			indicators.DetectVCPParams{
				Code:               g.cfg.Symbol,
				Lookback:           g.cfg.VCPLookback,
				MinContractions:    2,
				MaxLastContraction: 10.0,
				BaseLengthMin:      indicators.VCPDefaultBaseMinDays,
				BaseLengthMax:      indicators.VCPDefaultBaseMaxDays,
			},
		)
	}

	// 4a. Trend template fails -> NO_SETUP.
	if !tt.Passed() {
		base.State = StateNoSetup
		base.Strength = float64(ttGrade)
		base.TrendTemplatePass = false
		base.Grade = ttGrade
		return base
	}

	// VCP diagnostics attached to BUY/FORMING (nil when no VCP).
	if haveVCP {
		ageDays := vcp.BaseLengthDays
		depth := vcp.LastContractionPct
		dryup := vcp.VolumeDryup
		base.BaseAgeDays = &ageDays
		base.BaseDepthPct = &depth
		base.VolumeDryup = &dryup
	}
	base.Strength = float64(ttGrade)
	base.TrendTemplatePass = true
	base.Grade = ttGrade

	// --- attach the actionable trade plan ------------------------------------
	// A professional SEPA trader must be able to ACT from the watchlist, so every
	// flat trend-template-passing signal (forming AND buy) carries a reliable,
	// NON-NULL trade plan: pivot, stop, signed proximity, risk, %off-52wk-high,
	// vol_ratio, and the buy-readiness composite. Baseline SEPA leaves forming
	// signals with only strength=100; this layer adds the plan.
	g.attachTradePlan(&base, lastClose, vcp, haveVCP)

	// 4b. pivot>0 AND last_close >= pivot -> BUY.
	if g.pivotPrice.val > 0 && lastClose >= g.pivotPrice.val {
		base.State = StateBuy
		return base
	}

	base.State = StateForming
	return base
}

// attachHeldTradePlan attaches trade-plan metrics for a HELD position using the
// persisted entry pivot + stop.
func (g *Generator) attachHeldTradePlan(base *SignalSnapshot, lastClose float64) {
	pivot := g.pivotPrice.val
	stop := g.stopPrice.val
	if pivot > 0 {
		prox := indicators.ProximityToTriggerPct(pivot, lastClose)
		base.ProximityToTriggerP = &prox
		if stop > 0 && stop < pivot {
			risk := indicators.RiskPct(pivot, stop)
			base.RiskPct = &risk
		}
	}
	high52 := indicators.FiftyTwoWeekHigh(g.high, indicators.TTHighLowWindow)
	pctOff := indicators.PctOff52wkHigh(lastClose, high52)
	base.PctOff52wkH = &pctOff
	volRatio := indicators.VolumeRatio(g.volume, indicators.VolumeSMALookback)
	base.VolRatio = &volRatio
}

// attachTradePlan computes and attaches the actionable trade-plan fields onto a
// flat-book (forming/buy) signal.
//
// pivot/stop = the VCP base pivot/low when a VCP is detected, ELSE the swing-high/
// low over the last SwingPlanLookback (10) COMPLETED daily bars (the bar before
// the current/forming bar is treated as the most recent completed bar — we scan
// the full buffer tail as all stored bars are completed EOD bars). The fallback
// guarantees a non-null pivot>0 and stop in (0,pivot) for EVERY forming signal.
func (g *Generator) attachTradePlan(base *SignalSnapshot, lastClose float64, vcp indicators.VCPSnapshot, haveVCP bool) {
	var pivot, stop float64
	if haveVCP && vcp.PivotPrice > 0 {
		pivot = vcp.PivotPrice
		stop = vcp.BaseLowPrice
	}
	// Swing fallback (or VCP gave a non-positive low): highest high / lowest low
	// of the last 10 completed bars. EvaluateSignal's stored bars are all
	// completed EOD bars, so completedExclusive=false (include the latest).
	if pivot <= 0 {
		pivot = indicators.SwingHighPivot(g.high, indicators.SwingPlanLookback, false)
	}
	if stop <= 0 || stop >= pivot {
		stop = indicators.SwingLowStop(g.low, indicators.SwingPlanLookback, false)
	}
	// Final guards: pivot must be > 0; stop must be > 0 and strictly < pivot.
	if pivot <= 0 {
		pivot = lastClose // degenerate (no history) — keep non-null, > 0.
	}
	if stop <= 0 || stop >= pivot {
		// Last-resort stop a hair below the pivot so risk_pct stays finite/positive.
		stop = pivot * 0.93
	}

	base.PivotPrice = pyFloatRepr(pivot)
	base.StopPrice = pyFloatRepr(stop)

	prox := indicators.ProximityToTriggerPct(pivot, lastClose)
	base.ProximityToTriggerP = &prox

	risk := indicators.RiskPct(pivot, stop)
	base.RiskPct = &risk

	high52 := indicators.FiftyTwoWeekHigh(g.high, indicators.TTHighLowWindow)
	pctOff := indicators.PctOff52wkHigh(lastClose, high52)
	base.PctOff52wkH = &pctOff

	volRatio := indicators.VolumeRatio(g.volume, indicators.VolumeSMALookback)
	base.VolRatio = &volRatio

	depth := 0.0
	if haveVCP {
		depth = vcp.LastContractionPct
	}
	rsRank := 0
	if base.RSRank != nil {
		rsRank = *base.RSRank
	}
	readiness := indicators.BuyReadiness(indicators.BuyReadinessInputs{
		ProximityPct: prox,
		RSRank:       rsRank,
		HasVCP:       haveVCP,
		BaseDepthPct: depth,
		RiskPct:      risk,
	})
	base.BuyReadiness = &readiness
}

// StateSummary returns the 11-key UI summary. Flat-book price/grade fields are
// empty strings (None); held-book prices are str(Decimal). vcp_detected is
// (held) AND pivot>0.
func (g *Generator) StateSummary() StateSummary {
	flat := g.position == 0
	s := StateSummary{
		Symbol:        g.cfg.Symbol,
		Regime:        g.regime,
		MarketCapUSD:  g.marketCapUSD,
		InBlackout:    g.earningsBlackout,
		PositionQty:   g.position,
		BarsInHistory: len(g.close),
	}
	if !flat {
		s.EntryPrice = g.entryPrice.str
		s.StopPrice = g.stopPrice.str
		if g.grade != "" {
			s.CurrentGrade = string(g.grade)
		}
		if g.pivotPrice.val > 0 {
			s.VCPDetected = true
			s.PivotPrice = g.pivotPrice.str
		}
	}
	return s
}

// StateSummary is the JSON-safe light state. Optional string fields are "" to
// denote None (the JSON encoder emits null for them, see MarshalJSON in
// state.go).
type StateSummary struct {
	Symbol        string
	Regime        string
	MarketCapUSD  float64
	InBlackout    bool
	PositionQty   int
	EntryPrice    string // str(Decimal) or "" (None)
	StopPrice     string
	CurrentGrade  string // "A+"/"B" or "" (None)
	VCPDetected   bool
	PivotPrice    string
	BarsInHistory int
}
