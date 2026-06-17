package sectorrotation

import (
	"sort"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// EvaluateSignal returns one SectorRotationSignal per universe ETF, in universe
// declaration order.
//
// Warmup gate: if ANY symbol is still short of lookback+1 closes, ALL signals
// are emitted as NO_SETUP with rank=0 — matching OnBar's _has_full_warmup gate,
// so the UI never sees partial rankings that would flicker.
//
// Generation increments on EVERY call (even warmup) and is NOT persisted.
func (sg *SignalGenerator) EvaluateSignal(asOf time.Time) []domain.SectorRotationSignal {
	sg.intentGeneration++
	universe := sg.cfg.Universe
	topK := sg.cfg.TopK
	n := len(universe)

	// Warmup gate: all-NO_SETUP, rank 0, strength 0.
	if !sg.hasFullWarmup() {
		out := make([]domain.SectorRotationSignal, 0, n)
		for _, sym := range universe {
			it := domain.NewSectorRotationSignal()
			it.Symbol = sym
			it.State = domain.StateNoSetup
			it.Strength = 0.0
			it.ProximityToTriggerPct = nil
			it.UpdatedAt = asOf
			it.Generation = sg.intentGeneration
			it.Rank = 0
			it.TargetWeight = 0.0
			it.CurrentWeight = 0.0
			out = append(out, it)
		}
		return out
	}

	// Returns from history. old <= 0 -> return 0.0.
	returns := make(map[string]float64, n)
	for _, sym := range universe {
		h := sg.history[sym]
		old := h.front()
		new := h.back()
		if old <= 0 {
			returns[sym] = 0.0
		} else {
			returns[sym] = ratioReturn(old, new)
		}
	}

	// Rank descending; STABLE so ties keep universe order (the sort iterates
	// `universe` in order).
	rankedSyms := make([]string, n)
	copy(rankedSyms, universe)
	sort.SliceStable(rankedSyms, func(i, j int) bool {
		return returns[rankedSyms[i]] > returns[rankedSyms[j]]
	})
	rankOf := make(map[string]int, n)
	for i, s := range rankedSyms {
		rankOf[s] = i + 1
	}
	topSet := make(map[string]struct{}, topK)
	for _, s := range rankedSyms[:topK] {
		topSet[s] = struct{}{}
	}
	targetW := 1.0 / float64(topK)

	// current_weight = shares * last_close / equity (approximate).
	equity := sg.cfg.EquityProvider()
	currentWeights := make(map[string]float64, n)
	for _, sym := range universe {
		qty := sg.currentPositions[sym]
		var last float64
		if lc, ok := sg.lastClose[sym]; ok {
			last = lc.Float64()
		}
		if equity > 0 {
			currentWeights[sym] = float64(qty) * last / equity
		} else {
			currentWeights[sym] = 0.0
		}
	}

	out := make([]domain.SectorRotationSignal, 0, n)
	for _, sym := range universe {
		rank := rankOf[sym]
		_, inTop := topSet[sym]
		held := sg.currentPositions[sym] > 0

		var state domain.SignalState
		switch {
		case inTop && held:
			state = domain.StateHold
		case inTop && !held:
			state = domain.StateBuy
		case !inTop && held:
			state = domain.StateExit
		case rank <= topK+2:
			state = domain.StateForming
		default:
			state = domain.StateNoSetup
		}

		// Proximity: rank-positions distance below cutoff (negative = below
		// top-K). float((top_k - rank) / max(n, 1) * 100.0) — true (float)
		// division.
		denom := n
		if denom < 1 {
			denom = 1
		}
		proximity := float64(topK-rank) / float64(denom) * 100.0

		it := domain.NewSectorRotationSignal()
		it.Symbol = sym
		it.State = state
		it.Strength = domain.StrengthFromRank(rank, n)
		it.ProximityToTriggerPct = &proximity
		it.UpdatedAt = asOf
		it.Generation = sg.intentGeneration
		it.MomentumScore = returns[sym]
		it.Rank = rank
		if inTop {
			it.TargetWeight = targetW
		} else {
			it.TargetWeight = 0.0
		}
		it.CurrentWeight = currentWeights[sym]
		out = append(out, it)
	}
	return out
}
