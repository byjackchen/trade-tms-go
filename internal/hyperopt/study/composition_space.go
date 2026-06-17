package study

// composition_space.go is the BE-Space (Blueprint-Evolution space) for a
// COMPOSITION-tuning study (docs/concept-alignment.md §1.2, the "Optimize this
// Composition" path). Where the strategy SpaceBuilder searches a strategy's
// SIGNAL params, this space searches a Composition's CAPITAL shape — the member
// weights, the cash reserve, and the composite portfolio-level risk — leaving
// every member's signal params FIXED at its active params (locked decision 4).
//
// The genome (locked decision 1a, simplex-normalized) carries, in a fixed order:
//
//	weight.<strategy_id>   raw weight dim per ACTIVE member (default 0.05..1.0)
//	cash                   raw cash dim                     (default 0.00..0.30)
//	risk.single_name       single_name_pct                  (default 0.10..0.60)
//	risk.concentration     concentration_pct                (default 0.20..0.60)
//	risk.daily_loss        daily_loss_halt_pct              (default 0.02..0.15)
//
// Decoding NORMALIZES the raw weights + cash onto a simplex so that
// Σ(weight_i) + cash == 1 exactly and every point is feasible (no rejected
// trials): weight_i = raw_i / (Σraw_weights + raw_cash), cash likewise. The risk
// dims map straight through. The decoded blueprint is the TARGET Composition's
// members (same strategy ids + the same per-member param_set_id) with the
// proposed weights/cash/risk substituted in.

import (
	"fmt"
	"sort"

	"github.com/byjackchen/trade-tms-go/internal/composition"
	"github.com/byjackchen/trade-tms-go/internal/hyperopt/nsga2"
)

// Composition-space genome dimension names (the fixed, sorted layout).
const (
	compWeightPrefix = "weight."
	compCashDim      = "cash"
	compRiskSingle   = "risk.single_name"
	compRiskConc     = "risk.concentration"
	compRiskDaily    = "risk.daily_loss"
)

// CompositionRanges are the per-launch search ranges for a composition study
// (locked decision 2: GLOBAL defaults, overridable in the launch body). Every
// field is a [low, high] pair over the respective raw/risk dimension.
type CompositionRanges struct {
	WeightLow, WeightHigh float64 // per-member raw weight (default 0.05..1.0)
	CashLow, CashHigh     float64 // raw cash (default 0.00..0.30)
	SingleLow, SingleHigh float64 // single_name_pct (default 0.10..0.60)
	ConcLow, ConcHigh     float64 // concentration_pct (default 0.20..0.60)
	DailyLow, DailyHigh   float64 // daily_loss_halt_pct (default 0.02..0.15)
}

// DefaultCompositionRanges returns the locked global default ranges.
func DefaultCompositionRanges() CompositionRanges {
	return CompositionRanges{
		WeightLow: 0.05, WeightHigh: 1.0,
		CashLow: 0.0, CashHigh: 0.3,
		SingleLow: 0.10, SingleHigh: 0.60,
		ConcLow: 0.20, ConcHigh: 0.60,
		DailyLow: 0.02, DailyHigh: 0.15,
	}
}

// validate checks each range is a sane (low < high), in-(0,1] interval for the
// risk dims and a non-negative raw interval for weight/cash.
func (r CompositionRanges) validate() error {
	pairs := []struct {
		name     string
		lo, hi   float64
		unitHigh bool // cap high at 1.0 (risk fractions) vs raw (weight/cash)
	}{
		{"weight", r.WeightLow, r.WeightHigh, true},
		{"cash", r.CashLow, r.CashHigh, true},
		{"single_name", r.SingleLow, r.SingleHigh, true},
		{"concentration", r.ConcLow, r.ConcHigh, true},
		{"daily_loss", r.DailyLow, r.DailyHigh, true},
	}
	for _, p := range pairs {
		if p.lo < 0 || p.hi <= 0 {
			return fmt.Errorf("composition range %q: low/high must be non-negative/positive (got [%g, %g])", p.name, p.lo, p.hi)
		}
		if p.lo >= p.hi {
			return fmt.Errorf("composition range %q: low %g must be < high %g", p.name, p.lo, p.hi)
		}
		if p.unitHigh && p.hi > 1.0 {
			return fmt.Errorf("composition range %q: high %g must be <= 1.0", p.name, p.hi)
		}
	}
	return nil
}

// CompositionSpace is the BE-Space for a composition study: it owns the TARGET
// blueprint (its ACTIVE members fix the weight dims + the per-member param
// reference) and the assembled nsga2 search space, and decodes a candidate back
// into a concrete proposed Composition. It mirrors SpaceBuilder's surface so the
// coordinator can drive either kind through one seam.
type CompositionSpace struct {
	target  composition.Composition // the composition being tuned (active members fix the dims)
	active  []string                // ACTIVE member strategy ids in weight-dim order (sorted)
	ranges  CompositionRanges
	space   *nsga2.SearchSpace
	weightI map[string]int // strategy_id -> param index (for stable decode)
}

// NewCompositionSpace validates the target + ranges and assembles the BE-Space.
// The target must have >=1 ACTIVE member (those are the only weight dims; an
// inactive member keeps weight 0 and is not assembled). The dim order is: each
// active member's raw weight (members sorted by strategy_id), then cash, then the
// three risk dims — a fixed layout so a given seed reproduces an identical
// population trajectory (locked decision 1).
func NewCompositionSpace(target composition.Composition, ranges CompositionRanges) (*CompositionSpace, error) {
	if err := target.Validate(); err != nil {
		return nil, fmt.Errorf("hyperopt: composition study target: %w", err)
	}
	if err := ranges.validate(); err != nil {
		return nil, err
	}
	var active []string
	for _, m := range target.Members {
		if m.Active {
			active = append(active, m.StrategyID)
		}
	}
	if len(active) == 0 {
		return nil, fmt.Errorf("hyperopt: composition %q has no active members to tune", target.ID)
	}
	sort.Strings(active)

	var params []nsga2.Param
	weightI := make(map[string]int, len(active))
	for i, sid := range active {
		params = append(params, nsga2.FloatParam(compWeightPrefix+sid, ranges.WeightLow, ranges.WeightHigh))
		weightI[sid] = i
	}
	params = append(params,
		nsga2.FloatParam(compCashDim, ranges.CashLow, ranges.CashHigh),
		nsga2.FloatParam(compRiskSingle, ranges.SingleLow, ranges.SingleHigh),
		nsga2.FloatParam(compRiskConc, ranges.ConcLow, ranges.ConcHigh),
		nsga2.FloatParam(compRiskDaily, ranges.DailyLow, ranges.DailyHigh),
	)
	sp, err := nsga2.NewSearchSpace(params...)
	if err != nil {
		return nil, fmt.Errorf("hyperopt: building composition search space: %w", err)
	}
	return &CompositionSpace{
		target:  target,
		active:  active,
		ranges:  ranges,
		space:   sp,
		weightI: weightI,
	}, nil
}

// Space returns the assembled optimizer search space.
func (c *CompositionSpace) Space() *nsga2.SearchSpace { return c.space }

// Decode turns one optimizer candidate into a proposed Composition: the raw
// weight + cash dims are simplex-normalized (locked decision 1a) so Σweights +
// cash == 1; the risk dims map through. The proposed Composition keeps the
// target's id/name/version, every active member's strategy id + param_set_id
// (signal params FIXED, decision 4), and substitutes the normalized weights +
// cash + risk. Inactive members are dropped (only active members carry a weight
// dim). The returned Decoded carries the blueprint in Composition; its strategy
// Overrides are nil (signal params are not tuned here).
func (c *CompositionSpace) Decode(cand nsga2.Params) (Decoded, error) {
	raws := make(map[string]float64, len(c.active))
	var sumW float64
	for _, sid := range c.active {
		v, err := toFloat(cand[compWeightPrefix+sid])
		if err != nil {
			return Decoded{}, fmt.Errorf("hyperopt: composition decode weight %q: %w", sid, err)
		}
		raws[sid] = v
		sumW += v
	}
	rawCash, err := toFloat(cand[compCashDim])
	if err != nil {
		return Decoded{}, fmt.Errorf("hyperopt: composition decode cash: %w", err)
	}
	denom := sumW + rawCash
	if denom <= 0 {
		// All-zero raw draw (range low can be 0): degenerate. Fall back to an
		// equal-weight simplex with zero cash so the trial is still feasible.
		denom = float64(len(c.active))
		for _, sid := range c.active {
			raws[sid] = 1
		}
		rawCash = 0
	}

	single, err := toFloat(cand[compRiskSingle])
	if err != nil {
		return Decoded{}, fmt.Errorf("hyperopt: composition decode single_name: %w", err)
	}
	conc, err := toFloat(cand[compRiskConc])
	if err != nil {
		return Decoded{}, fmt.Errorf("hyperopt: composition decode concentration: %w", err)
	}
	daily, err := toFloat(cand[compRiskDaily])
	if err != nil {
		return Decoded{}, fmt.Errorf("hyperopt: composition decode daily_loss: %w", err)
	}

	prop := composition.Composition{
		ID:          c.target.ID,
		Name:        c.target.Name,
		Description: c.target.Description,
		CashPct:     rawCash / denom,
		Risk: composition.Risk{
			SingleNamePct:    single,
			ConcentrationPct: conc,
			DailyLossHaltPct: daily,
			MaxGrossPct:      c.target.Risk.MaxGrossPct,
			MaxPositions:     c.target.Risk.MaxPositions,
		},
		Version: c.target.Version,
	}
	// Preserve member identity (strategy id + param_set_id) from the target's
	// ACTIVE members; substitute the normalized weight. Iterate the target's
	// member order so the assembled allocator order is stable.
	for _, m := range c.target.Members {
		if !m.Active {
			continue
		}
		prop.Members = append(prop.Members, composition.Member{
			StrategyID: m.StrategyID,
			Weight:     raws[m.StrategyID] / denom,
			Active:     true,
			ParamSetID: m.ParamSetID,
		})
	}
	return Decoded{Composition: &prop}, nil
}

// RecordedParams renders a candidate into the trial artifact params map: the flat
// normalized blueprint values the UI shows + promote reads (per-member weight,
// cash, the three risk caps). Unlike the strategy path's pre-clamp recording,
// these are the NORMALIZED, post-decode values (what actually ran and what
// promote writes in place) — the simplex normalization is the whole point.
func (c *CompositionSpace) RecordedParams(cand nsga2.Params) map[string]any {
	dec, err := c.Decode(cand)
	if err != nil || dec.Composition == nil {
		// Best-effort: a decode failure means the trial FAILs; record the raw dims.
		out := make(map[string]any, len(cand))
		for k, v := range cand {
			out[k] = v
		}
		return out
	}
	m := dec.Composition
	out := map[string]any{
		"cash_pct":            m.CashPct,
		"single_name_pct":     m.Risk.SingleNamePct,
		"concentration_pct":   m.Risk.ConcentrationPct,
		"daily_loss_halt_pct": m.Risk.DailyLossHaltPct,
	}
	weights := make(map[string]any, len(m.Members))
	for _, mem := range m.Members {
		weights[mem.StrategyID] = mem.Weight
	}
	out["weights"] = weights
	return out
}
