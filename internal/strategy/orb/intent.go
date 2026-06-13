package orb

// intent.go ports evaluate_intent (intraday_breakout/signal.py:307-399) — the
// single per-symbol UI snapshot (NOT a list, unlike sector_rotation). Each call
// increments the generation counter.

import (
	"math"
	"time"
)

// EvaluateIntent returns the typed ORB intent as of asOf, exactly matching
// signal.py:307-399. The short-circuit order is: NO_SETUP (range not locked) ->
// HOLD (in position) -> NO_SETUP (past EOD) -> FORMING (no last close) ->
// BUY (last > orb_high) -> FORMING (otherwise). generation increments first.
func (g *Generator) EvaluateIntent(asOf time.Time) SignalIntent {
	g.intentGeneration++

	asOfLocal := asOf.In(g.loc)

	var windowEndLocal *time.Time
	var windowEndUTC *time.Time
	if g.currentSessionDate != nil {
		h, m, _ := parseHHMM(g.cfg.EODExitTime)
		wl := time.Date(g.currentSessionDate.year, g.currentSessionDate.month,
			g.currentSessionDate.day, h, m, 0, 0, g.loc)
		windowEndLocal = &wl
		wu := wl.UTC()
		windowEndUTC = &wu
	}

	base := SignalIntent{
		Symbol:         g.cfg.Symbol,
		UpdatedAt:      asOf,
		Generation:     g.intentGeneration,
		StrategyID:     StrategyID,
		ORBHigh:        decPtrString(g.rangeHigh),
		ORBLow:         decPtrString(g.rangeLow),
		ATRAtOpen:      "", // reserved, always nil
		EntryWindowEnd: windowEndUTC,
	}

	// 1. Range not locked / absent -> NO_SETUP.
	if g.rangeHigh == nil || g.rangeLow == nil || !g.rangeLocked {
		base.State = StateNoSetup
		base.Strength = 0.0
		base.ProximityToTriggerPct = nil
		return base
	}

	// 2. Held -> HOLD.
	if g.positionQty > 0 {
		base.State = StateHold
		base.Strength = 100.0
		base.ProximityToTriggerPct = nil
		return base
	}

	// 3. Flat, past EOD window -> NO_SETUP (no new entries allowed).
	if windowEndLocal != nil && !asOfLocal.Before(*windowEndLocal) {
		base.State = StateNoSetup
		base.Strength = 0.0
		base.ProximityToTriggerPct = nil
		return base
	}

	// 4. Flat, in window, no last close -> FORMING.
	if g.lastSeenClose == nil {
		base.State = StateForming
		base.Strength = 50.0
		base.ProximityToTriggerPct = nil
		return base
	}

	last := *g.lastSeenClose
	orbHigh := *g.rangeHigh
	orbLow := *g.rangeLow

	// 5. last > orb_high -> BUY.
	if last.cmp(orbHigh) > 0 {
		prox := last.sub(orbHigh).div(orbHigh).mul(decFromInt(100)).float64()
		base.State = StateBuy
		base.Strength = 100.0
		base.ProximityToTriggerPct = &prox
		return base
	}

	// 6. FORMING (last == orb_high also lands here): strength = position in range.
	rangeWidth := orbHigh.sub(orbLow)
	var strength float64
	if rangeWidth.cmp(decFromInt(0)) > 0 {
		strength = last.sub(orbLow).div(rangeWidth).mul(decFromInt(100)).float64()
		strength = math.Max(0.0, math.Min(100.0, strength))
	} else {
		strength = 50.0
	}
	prox := last.sub(orbHigh).div(orbHigh).mul(decFromInt(100)).float64()
	base.State = StateForming
	base.Strength = strength
	base.ProximityToTriggerPct = &prox
	return base
}

// decPtrString renders a *pydec as str(Decimal), "" when nil.
func decPtrString(d *pydec) string {
	if d == nil {
		return ""
	}
	return d.String()
}
