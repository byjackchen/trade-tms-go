package study

// space.go bridges the loader's search-space semantics (hyperopt §2.3/§2.6) to
// the self-written NSGA-II optimizer (internal/hyperopt/nsga2). It builds an
// ordered nsga2.SearchSpace from the embedded baseline JSON of one (or all
// three, for "joint") strategies, preserving the EXACT parameter order
// suggest_with / suggest_joint_params iterate in — that order fixes the genome
// layout and therefore the seeded PRNG consumption order (so a given seed
// reproduces an identical population trajectory; locked decision 1).
//
// The optimizer searches over the prefixed param names ("<strategy>.<param>").
// Decoding a candidate back into the per-fold
// backtest overrides reuses the loader's SuggestWith constraint pass: the raw
// genome values feed a synthetic trial whose SuggestFloat/SuggestInt simply
// echo the optimizer's already-decoded values, and SuggestWith then applies the
// file-order clamp constraints (e.g. pairs exit_z <- min(exit_z, entry_z-0.1)).
// This keeps the constraint semantics single-sourced in internal/hyperopt and
// guarantees the backtest runs with the clamped values while the OPTUNA-recorded
// params (what best_params promotes) stay pre-clamp (§2.3 note, Q5: bug-for-bug).

import (
	"fmt"

	"github.com/byjackchen/trade-tms-go/internal/composition"
	"github.com/byjackchen/trade-tms-go/internal/hyperopt"
	"github.com/byjackchen/trade-tms-go/internal/hyperopt/nsga2"
)

// SpaceBuilder turns a study strategy ("sepa"|"sector_rotation"|"pairs"|"joint")
// into an nsga2 search space plus the metadata needed to decode candidates back
// into per-strategy override maps. It owns the parsed baseline StrategyParams so
// decoding never re-reads the filesystem.
type SpaceBuilder struct {
	// Strategy is the study strategy selector.
	Strategy string
	// order is the list of sub-strategies sampled, in suggest order
	// (["sepa"] etc., or the joint triple sepa->sector_rotation->pairs).
	order []string
	// params holds each sub-strategy's parsed baseline params, keyed by name.
	params map[string]*hyperopt.StrategyParams
	// space is the assembled optimizer search space (prefixed names, ordered).
	space *nsga2.SearchSpace
}

// strategyOrder returns the ordered sub-strategy list a study strategy samples.
// "joint" samples sepa -> sector_rotation -> pairs (the fixed order Optuna's
// suggest_joint_params consumes; §2.6). The single strategies sample themselves.
func strategyOrder(strategy string) ([]string, error) {
	switch strategy {
	case "sepa", "sector_rotation", "pairs":
		return []string{strategy}, nil
	case "joint":
		return append([]string(nil), hyperopt.SearchSpaceStrategies...), nil
	default:
		return nil, fmt.Errorf("unknown strategy: %s", strategy)
	}
}

// NewSpaceBuilder parses the baseline params for every sub-strategy the study
// samples and assembles the ordered nsga2 search space. The optimizer parameter
// names are PREFIXED "<sub>.<param>" (matching the Optuna names suggest_with
// passes to trial.suggest_*), and only parameters carrying a search range are
// included — exactly the keys suggest_with returns.
func NewSpaceBuilder(strategy string) (*SpaceBuilder, error) {
	order, err := strategyOrder(strategy)
	if err != nil {
		return nil, err
	}
	b := &SpaceBuilder{
		Strategy: strategy,
		order:    order,
		params:   make(map[string]*hyperopt.StrategyParams, len(order)),
	}
	var params []nsga2.Param
	for _, sub := range order {
		sp, err := hyperopt.LoadBaselineParams(sub)
		if err != nil {
			return nil, fmt.Errorf("hyperopt: loading baseline for %q: %w", sub, err)
		}
		b.params[sub] = sp
		for _, spec := range sp.Parameters {
			if spec.Search == nil {
				continue // static param: not searched (suggest_with skips it)
			}
			full := sub + "." + spec.Name
			switch spec.Type {
			case "float":
				params = append(params, nsga2.FloatParam(full, spec.Search.Low, spec.Search.High))
			case "int":
				params = append(params, nsga2.IntParam(full, int64(spec.Search.Low), int64(spec.Search.High)))
			default:
				return nil, fmt.Errorf("hyperopt: %s: search on non-numeric type %q", full, spec.Type)
			}
		}
	}
	if len(params) == 0 {
		return nil, fmt.Errorf("hyperopt: strategy %q has no searchable parameters", strategy)
	}
	sp, err := nsga2.NewSearchSpace(params...)
	if err != nil {
		return nil, fmt.Errorf("hyperopt: building search space: %w", err)
	}
	b.space = sp
	return b, nil
}

// Space returns the assembled optimizer search space.
func (b *SpaceBuilder) Space() *nsga2.SearchSpace { return b.space }

// Order returns the ordered sub-strategy list (single element, or the joint
// triple). The slice is a copy; callers may not mutate the builder's state.
func (b *SpaceBuilder) Order() []string { return append([]string(nil), b.order...) }

// RecordedParams renders the optimizer's decoded candidate into the trial
// artifact params map: for a single strategy, the flat unprefixed {param: value};
// for joint, the nested {sub: {param: value}}. Values are the OPTUNA-recorded
// (pre-clamp) values — int params as whole float64s (the Optuna shape). This is
// the studySpace surface the coordinator projects each trial through.
func (b *SpaceBuilder) RecordedParams(cand nsga2.Params) map[string]any {
	if len(b.order) == 1 {
		sub := b.order[0]
		out := make(map[string]any, len(cand))
		for full, v := range cand {
			out[stripPrefix(full, sub)] = v
		}
		return out
	}
	nested := make(map[string]any, len(b.order))
	for _, sub := range b.order {
		nested[sub] = map[string]any{}
	}
	for full, v := range cand {
		sub := subStrategyOf(full, b.order)
		if sub == "" {
			continue
		}
		nested[sub].(map[string]any)[stripPrefix(full, sub)] = v
	}
	return nested
}

// echoTrial is a TrialLike whose Suggest* return the optimizer's already-decoded
// values (looked up by prefixed name). It is the bridge that lets SuggestWith
// apply the file-order constraint clamps over the genome's values without
// re-sampling. Suggest* are called by SuggestWith in file order, exactly the
// order the genome was laid out, so every lookup hits.
type echoTrial struct {
	vals map[string]float64
}

func (e echoTrial) SuggestFloat(name string, _, _ float64) float64 {
	return e.vals[name]
}

func (e echoTrial) SuggestInt(name string, _, _ int64) int64 {
	return int64(e.vals[name])
}

// Decoded is the result of decoding one optimizer candidate: the OPTUNA-style
// recorded params (prefixed names -> raw pre-clamp values, what trial_*.json
// stores and best_params promotes) plus the per-sub-strategy CLAMPED override
// maps (unprefixed) the backtest actually runs with.
type Decoded struct {
	// RecordedParams is the prefixed name -> raw value map (pre-constraint-clamp),
	// the Optuna-recorded params (§2.3, Q5). Floats stay float64; ints are whole
	// float64s (the JSON shape Optuna records).
	RecordedParams map[string]float64
	// Overrides maps each sub-strategy to its CLAMPED unprefixed param map (the
	// values the backtest runs with).
	Overrides map[string]map[string]float64
	// Composition, when non-nil, is the proposed blueprint a COMPOSITION-tuning
	// study decoded (the BE-Space path, composition_space.go): the target's active
	// members with normalized weights + cash + risk substituted in. The strategy
	// SIGNAL params are FIXED (decision 4), so RecordedParams/Overrides are unused
	// on this path. nil for a strategy study.
	Composition *composition.Composition
}

// Decode turns an nsga2 candidate (decoded Params: name -> any, ints as int64,
// floats as float64, all keyed by PREFIXED name) into Decoded. It groups the
// values by sub-strategy, then runs each sub-strategy's SuggestWith over an
// echoTrial so the file-order constraints clamp the override map while the
// recorded params remain the raw pre-clamp values.
func (b *SpaceBuilder) Decode(cand nsga2.Params) (Decoded, error) {
	recorded := make(map[string]float64, len(cand))
	// Group raw values by sub-strategy, keyed by prefixed name (echoTrial lookup).
	perSub := make(map[string]map[string]float64, len(b.order))
	for _, sub := range b.order {
		perSub[sub] = map[string]float64{}
	}
	for name, v := range cand {
		f, err := toFloat(v)
		if err != nil {
			return Decoded{}, fmt.Errorf("hyperopt: decoding param %q: %w", name, err)
		}
		recorded[name] = f
		sub := subStrategyOf(name, b.order)
		if sub == "" {
			return Decoded{}, fmt.Errorf("hyperopt: param %q has no known strategy prefix", name)
		}
		perSub[sub][name] = f
	}

	overrides := make(map[string]map[string]float64, len(b.order))
	for _, sub := range b.order {
		sp := b.params[sub]
		clamped, err := hyperopt.SuggestWith(sp, echoTrial{vals: perSub[sub]})
		if err != nil {
			return Decoded{}, fmt.Errorf("hyperopt: %s constraints: %w", sub, err)
		}
		overrides[sub] = clamped
	}
	return Decoded{RecordedParams: recorded, Overrides: overrides}, nil
}

// subStrategyOf returns which sub-strategy a prefixed param name belongs to, by
// matching "<sub>." against the known order. Strategy names contain no dots, so
// the first segment is unambiguous.
func subStrategyOf(name string, order []string) string {
	for _, sub := range order {
		if len(name) > len(sub)+1 && name[:len(sub)+1] == sub+"." {
			return sub
		}
	}
	return ""
}

// toFloat coerces a decoded nsga2 param value (int64 or float64) to float64.
func toFloat(v any) (float64, error) {
	switch x := v.(type) {
	case float64:
		return x, nil
	case int64:
		return float64(x), nil
	case int:
		return float64(x), nil
	default:
		return 0, fmt.Errorf("unexpected param type %T", v)
	}
}
