package hyperopt

// composition_space.go is the Composition-hyperopt foundation: it turns a
// composition.Composition into the tunable parameter list the existing sampler /
// NSGA-II machinery already consumes, and decodes a sampled vector back into a
// concrete (members, cash_pct, risk) the engine drops in (docs/concept-alignment.md
// §1.2, the Composition "Optimize" path). It is PURELY ADDITIVE — the single- and
// joint-strategy spaces in search_spaces.go / study.SpaceBuilder are untouched.
//
// The search dimensions (LOCKED decision 1a + global default ranges, decision 2):
//
//   - one RAW WEIGHT dim per ACTIVE member, named "weight.<strategy_id>", range
//     [0.05, 1.0] — an unnormalized magnitude;
//   - one RAW CASH dim, named "cash", range [0.0, 0.3];
//   - three composite RISK dims, named "risk.single_name_pct" /
//     "risk.concentration_pct" / "risk.daily_loss_halt_pct", with the global
//     default ranges below.
//
// DecodeComposition then NORMALIZES the raw weights + raw cash onto a simplex so
// Σ(weights) + cash == 1 exactly (decision 1a) — always feasible, no rejected
// samples. Risk dims pass through verbatim. Per decision 4 the member param refs
// (ParamSetID) and Active flags are FIXED from the source Composition; this space
// tunes weights / cash / risk ONLY, never strategy params.

import (
	"fmt"
	"sort"

	"github.com/byjackchen/trade-tms-go/internal/composition"
	"github.com/byjackchen/trade-tms-go/internal/hyperopt/nsga2"
)

// Composition search-dimension name prefixes/keys. Member weight dims are
// "weight.<strategy_id>"; cash is the bare "cash"; risk dims are
// "risk.<field>". Strategy ids contain no dots, so the prefixes are unambiguous.
const (
	compWeightPrefix    = "weight."
	compCashName        = "cash"
	compRiskSingleName  = "risk.single_name_pct"
	compRiskConcentNm   = "risk.concentration_pct"
	compRiskDailyHaltNm = "risk.daily_loss_halt_pct"
)

// CompositionRanges holds the [low,high] search ranges for the Composition space.
// All ranges are GLOBAL DEFAULTS (DefaultCompositionRanges) but overridable in a
// launch request (decision 2). RawWeight applies to EVERY active member's raw
// weight dim; the per-member normalization (decision 1a) makes a shared range
// well-defined regardless of member count.
type CompositionRanges struct {
	RawWeight        SearchSpec `json:"raw_weight"`
	RawCash          SearchSpec `json:"raw_cash"`
	SingleNamePct    SearchSpec `json:"single_name_pct"`
	ConcentrationPct SearchSpec `json:"concentration_pct"`
	DailyLossHaltPct SearchSpec `json:"daily_loss_halt_pct"`
}

// DefaultCompositionRanges returns the LOCKED global default search ranges
// (decision 2): per-member raw weight 0.05–1.0, raw cash 0.0–0.3, single_name_pct
// 0.10–0.60, concentration_pct 0.20–0.60, daily_loss_halt_pct 0.02–0.15.
func DefaultCompositionRanges() CompositionRanges {
	return CompositionRanges{
		RawWeight:        SearchSpec{Low: 0.05, High: 1.0},
		RawCash:          SearchSpec{Low: 0.0, High: 0.3},
		SingleNamePct:    SearchSpec{Low: 0.10, High: 0.60},
		ConcentrationPct: SearchSpec{Low: 0.20, High: 0.60},
		DailyLossHaltPct: SearchSpec{Low: 0.02, High: 0.15},
	}
}

// CompositionSpace is the assembled Composition search space plus the metadata
// DecodeComposition needs to map a sampled vector back to a concrete Composition.
// It owns a copy of the source Composition so decoding carries through every
// FIXED attribute (member ParamSetID/Active, inactive members, id/name, optional
// risk caps) without re-reading anything.
type CompositionSpace struct {
	// source is the Composition the space was built from (copied defensively).
	source composition.Composition
	// activeMembers lists the active members in deterministic order (by
	// strategy_id), fixing the weight-dim gene layout and PRNG consumption order.
	activeMembers []composition.Member
	// ranges are the search ranges used (defaults, or the caller's overrides).
	ranges CompositionRanges
	// space is the assembled nsga2 search space (ordered: per-member weights,
	// then cash, then the three risk dims).
	space *nsga2.SearchSpace
}

// NewCompositionSpace builds the Composition search space for comp using ranges
// (pass DefaultCompositionRanges() for the global defaults). It samples one raw
// weight per ACTIVE member, one raw cash, and the three risk dims. Errors if comp
// has no active members (nothing to tune).
func NewCompositionSpace(comp composition.Composition, ranges CompositionRanges) (*CompositionSpace, error) {
	// Defensive copy: caller may mutate comp / its Members slice afterwards.
	cp := comp
	cp.Members = append([]composition.Member(nil), comp.Members...)

	var active []composition.Member
	for _, m := range cp.Members {
		if m.Active {
			active = append(active, m)
		}
	}
	if len(active) == 0 {
		return nil, fmt.Errorf("composition %q: no active members to tune", comp.ID)
	}
	// Deterministic dim order: by strategy_id. Duplicates are already rejected by
	// composition.Validate, so the sort is a total order over the active set.
	sort.Slice(active, func(i, j int) bool { return active[i].StrategyID < active[j].StrategyID })

	params := make([]nsga2.Param, 0, len(active)+4)
	for _, m := range active {
		params = append(params, nsga2.FloatParam(compWeightPrefix+m.StrategyID, ranges.RawWeight.Low, ranges.RawWeight.High))
	}
	params = append(params, nsga2.FloatParam(compCashName, ranges.RawCash.Low, ranges.RawCash.High))
	params = append(params,
		nsga2.FloatParam(compRiskSingleName, ranges.SingleNamePct.Low, ranges.SingleNamePct.High),
		nsga2.FloatParam(compRiskConcentNm, ranges.ConcentrationPct.Low, ranges.ConcentrationPct.High),
		nsga2.FloatParam(compRiskDailyHaltNm, ranges.DailyLossHaltPct.Low, ranges.DailyLossHaltPct.High),
	)

	sp, err := nsga2.NewSearchSpace(params...)
	if err != nil {
		return nil, fmt.Errorf("composition %q: building search space: %w", comp.ID, err)
	}
	return &CompositionSpace{
		source:        cp,
		activeMembers: active,
		ranges:        ranges,
		space:         sp,
	}, nil
}

// Space returns the assembled optimizer search space.
func (s *CompositionSpace) Space() *nsga2.SearchSpace { return s.space }

// Ranges returns the search ranges this space was built with (for persistence to
// hyperopt_studies.search_config, decision 2).
func (s *CompositionSpace) Ranges() CompositionRanges { return s.ranges }

// DecodeComposition maps a sampled candidate (PREFIXED dim name -> value, floats
// as float64 per nsga2 decode) into a concrete Composition: active members with
// NORMALIZED weights, the normalized cash_pct, and the sampled risk — carrying
// through every FIXED attribute from the source.
//
// Normalization (LOCKED decision 1a): with raw weights w_i (per active member)
// and raw cash c, total = Σ w_i + c, then weight_i = w_i / total and
// cash = c / total, guaranteeing Σ(weights) + cash == 1 (within float epsilon)
// and always feasible. Inactive members are dropped from the decoded result
// (they carry no weight); ParamSetID is preserved on the active members.
func (s *CompositionSpace) DecodeComposition(cand nsga2.Params) (composition.Composition, error) {
	rawCash, err := candFloat(cand, compCashName)
	if err != nil {
		return composition.Composition{}, err
	}

	raw := make([]float64, len(s.activeMembers))
	total := rawCash
	for i, m := range s.activeMembers {
		w, err := candFloat(cand, compWeightPrefix+m.StrategyID)
		if err != nil {
			return composition.Composition{}, err
		}
		raw[i] = w
		total += w
	}
	if total <= 0 {
		// Degenerate only if every raw dim sampled exactly 0; the default ranges
		// keep per-member weight low>=0.05 so this is unreachable in practice, but
		// guard against an all-zero override range rather than divide by zero.
		return composition.Composition{}, fmt.Errorf("composition %q: raw weights + cash sum to %v; cannot normalize", s.source.ID, total)
	}

	members := make([]composition.Member, len(s.activeMembers))
	for i, m := range s.activeMembers {
		members[i] = composition.Member{
			StrategyID: m.StrategyID,
			Weight:     raw[i] / total,
			Active:     true,
			ParamSetID: m.ParamSetID,
		}
	}

	single, err := candFloat(cand, compRiskSingleName)
	if err != nil {
		return composition.Composition{}, err
	}
	concent, err := candFloat(cand, compRiskConcentNm)
	if err != nil {
		return composition.Composition{}, err
	}
	dailyHalt, err := candFloat(cand, compRiskDailyHaltNm)
	if err != nil {
		return composition.Composition{}, err
	}

	out := s.source
	out.Members = members
	out.CashPct = rawCash / total
	out.Risk = composition.Risk{
		SingleNamePct:    single,
		ConcentrationPct: concent,
		DailyLossHaltPct: dailyHalt,
		// Optional caps are FIXED carry-through from the source Composition.
		MaxGrossPct:  s.source.Risk.MaxGrossPct,
		MaxPositions: s.source.Risk.MaxPositions,
	}
	return out, nil
}

// candFloat reads a float64 dimension from a decoded nsga2 candidate, erroring if
// the key is absent or not numeric.
func candFloat(cand nsga2.Params, name string) (float64, error) {
	v, ok := cand[name]
	if !ok {
		return 0, fmt.Errorf("composition decode: missing dimension %q", name)
	}
	switch x := v.(type) {
	case float64:
		return x, nil
	case int64:
		return float64(x), nil
	case int:
		return float64(x), nil
	default:
		return 0, fmt.Errorf("composition decode: dimension %q has unexpected type %T", name, v)
	}
}
