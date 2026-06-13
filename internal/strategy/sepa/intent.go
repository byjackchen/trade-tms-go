package sepa

// intent.go ports evaluate_intent (signal.py:398-513) and state_summary
// (signal.py:515-539) — the read-only UI projections. evaluate_intent's only
// state mutation is the monotonic _intent_generation increment (the reference
// docstring claims "does NOT mutate" but the counter does advance, asserted by
// test_intent.py:125-130); we replicate that.
//
// Deliberate asymmetry [MUST-MATCH spec §12 #7]: evaluate_intent's VCP runs on
// the FULL klines (INCLUDING the current bar), unlike the entry path which
// excludes it. This is a UI-only path, not a trading decision; do not "fix" it.

import (
	"time"

	"github.com/byjackchen/trade-tms-go/internal/indicators"
)

// EvaluateIntent returns a typed UI snapshot of the current setup state
// (signal.py:398-513). asOf is the snapshot timestamp (the bar ts in the
// streaming path). It increments the generation counter on every call.
func (g *Generator) EvaluateIntent(asOf time.Time) SignalIntent {
	g.intentGeneration++

	pivotStr := ""
	if g.pivotPrice.val > 0 {
		pivotStr = g.pivotPrice.str
	}
	stopStr := ""
	if g.stopPrice.val > 0 {
		stopStr = g.stopPrice.str
	}

	base := SignalIntent{
		Symbol:     g.cfg.Symbol,
		UpdatedAt:  asOf,
		Generation: g.intentGeneration,
		StrategyID: StrategyID,
		PivotPrice: pivotStr,
		StopPrice:  stopStr,
	}

	n := len(g.close)

	// 1. Empty or < 50 bars -> NO_SETUP (signal.py:421-430).
	if n < 50 {
		base.State = StateNoSetup
		base.Strength = 0.0
		base.TrendTemplatePass = false
		base.Grade = 0
		return base
	}

	lastClose := g.close[n-1] // Decimal(str(close.iloc[-1]))

	// 2. Held + stop>0 + last_close < stop (STRICT <) -> STOP_HIT
	//    (signal.py:434-446). Note: on_bar uses <=; intent uses < (the
	//    documented predicate mismatch, spec §8.8 [IMPROVE] — replicated).
	if g.position != 0 && g.stopPrice.val > 0 && lastClose < g.stopPrice.val {
		base.State = StateStopHit
		base.Strength = 0.0
		base.TrendTemplatePass = false
		base.Grade = 0
		return base
	}

	// 3. Held normally -> HOLD (signal.py:449-457). tt_pass hard-coded true.
	if g.position != 0 {
		base.State = StateHold
		base.Strength = 50.0
		base.TrendTemplatePass = true
		base.Grade = 0
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

	// 4a. Trend template fails -> NO_SETUP (signal.py:478-486).
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

	// 4b. pivot>0 AND last_close >= pivot -> BUY (signal.py:488-502).
	if g.pivotPrice.val > 0 && lastClose >= g.pivotPrice.val {
		prox := (lastClose - g.pivotPrice.val) / g.pivotPrice.val * 100
		base.State = StateBuy
		base.ProximityToTriggerP = &prox
		return base
	}

	// 4c. Otherwise FORMING; proximity only when pivot>0 (signal.py:504-513).
	if g.pivotPrice.val > 0 {
		prox := (lastClose - g.pivotPrice.val) / g.pivotPrice.val * 100
		base.ProximityToTriggerP = &prox
	}
	base.State = StateForming
	return base
}

// StateSummary returns the 11-key UI summary (signal.py:515-539). Flat-book
// price/grade fields are empty strings (the reference's None); held-book prices
// are str(Decimal). vcp_detected is (held) AND pivot>0.
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

// StateSummary is the JSON-safe light state (signal.py:515-539). Optional
// string fields are "" to denote the reference's None (the JSON encoder emits
// null for them, see MarshalJSON in state.go).
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
