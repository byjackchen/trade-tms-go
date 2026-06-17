package pairs

// observability.go: read-side surfaces — EvaluateIntent and StateSummary
// (spec §9). These NEVER affect trading.

import (
	"fmt"
	"math"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// EvaluateIntent returns exactly 2*N intents for N configured pairs, in pair
// order (long leg then short leg). Pure read of telemetry + state (spec §9.1).
// It increments the generation counter at the TOP of the call (before building
// the list), starting at 1 on the first call.
//
// Inputs per pair default to z=0.0, beta=1.0, state=FLAT when the pair is still
// in warmup (telemetry not yet computed). Note the intent thresholds use
// >=/<= (unlike the strict trading comparisons) — intentional and pinned by
// tests (spec §9.1).
func (g *Generator) EvaluateIntent(asOf time.Time) []domain.PairsSignalIntent {
	g.intentGeneration++
	entryZ := g.cfg.EntryZ
	exitZ := g.cfg.ExitZ
	out := make([]domain.PairsSignalIntent, 0, 2*len(g.cfg.Pairs))
	for _, pair := range g.cfg.Pairs {
		key := pair.Key()
		z := 0.0
		if g.hasZ[key] {
			z = g.latestZ[key]
		}
		beta := 1.0
		if g.hasBeta[key] {
			beta = g.latestBeta[key]
		}
		pairState := StateFlat
		if s, ok := g.pairState[key]; ok {
			pairState = s
		}
		absZ := math.Abs(z)

		var state domain.SignalState
		var proximity *float64
		if pairState == StateFlat {
			switch {
			case absZ >= entryZ:
				state = domain.StateBuy
				if entryZ > 0 {
					v := (absZ - entryZ) / entryZ * 100.0
					proximity = &v
				}
			case absZ >= 0.7*entryZ:
				state = domain.StateForming
				if entryZ > 0 {
					v := (absZ - entryZ) / entryZ * 100.0
					proximity = &v
				}
			default:
				state = domain.StateNoSetup
			}
		} else { // LONG_SPREAD or SHORT_SPREAD = in a position
			if absZ <= exitZ {
				state = domain.StateExit
				v := (absZ - exitZ) / math.Max(exitZ, 0.1) * 100.0
				proximity = &v
			} else {
				state = domain.StateHold
			}
		}

		strength := domain.StrengthFromZ(absZ)
		pairID := fmt.Sprintf("%s/%s", pair.LongLeg, pair.ShortLeg)
		for _, leg := range [2]struct {
			symbol string
			role   domain.LegRole
		}{
			{pair.LongLeg, domain.LegLong},
			{pair.ShortLeg, domain.LegShort},
		} {
			it := domain.NewPairsSignalIntent()
			it.Symbol = leg.symbol
			it.State = state
			it.Strength = strength
			it.ProximityToTriggerPct = proximity
			it.UpdatedAt = asOf
			it.Generation = g.intentGeneration
			it.PairID = pairID
			it.LegRole = leg.role
			it.ZScore = z
			it.ZEntryThreshold = entryZ
			it.ZExitThreshold = exitZ
			it.HedgeRatio = beta
			out = append(out, it)
		}
	}
	return out
}

// PairSummary is one entry of StateSummary. current_z / current_beta are nil
// until the pair's first successful evaluation (spec §9.2).
type PairSummary struct {
	LongLeg     string   `json:"long_leg"`
	ShortLeg    string   `json:"short_leg"`
	State       string   `json:"state"`
	CurrentZ    *float64 `json:"current_z"`
	CurrentBeta *float64 `json:"current_beta"`
	LongLegQty  int64    `json:"long_leg_qty"`  // signed; negative when shorted
	ShortLegQty int64    `json:"short_leg_qty"` // signed
}

// StateSummary returns per-pair state for the UI, one entry per configured pair
// in config order (spec §9.2). JSON-serializable primitives.
func (g *Generator) StateSummary() map[string][]PairSummary {
	pairs := make([]PairSummary, 0, len(g.cfg.Pairs))
	for _, p := range g.cfg.Pairs {
		key := p.Key()
		state := StateFlat
		if s, ok := g.pairState[key]; ok {
			state = s
		}
		var z, beta *float64
		if g.hasZ[key] {
			v := g.latestZ[key]
			z = &v
		}
		if g.hasBeta[key] {
			v := g.latestBeta[key]
			beta = &v
		}
		pairs = append(pairs, PairSummary{
			LongLeg:     p.LongLeg,
			ShortLeg:    p.ShortLeg,
			State:       string(state),
			CurrentZ:    z,
			CurrentBeta: beta,
			LongLegQty:  g.legPosition[p.LongLeg],
			ShortLegQty: g.legPosition[p.ShortLeg],
		})
	}
	return map[string][]PairSummary{"pairs": pairs}
}
