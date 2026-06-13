package hyperopt

// search_spaces.go ports src/research/search_spaces.py (spec §2.6 [MUST-MATCH])
// plus the baseline param JSONs (§2.5). The registry keys are exactly
// {"sepa","sector_rotation","pairs"} (intraday_breakout exists in baseline but
// is NOT registered for hyperopt — ADR-006 "Known limitations"). The baseline
// JSON files are embedded so the loader resolves them without filesystem state.

import (
	"embed"
	"fmt"
	"sort"
)

//go:embed baseline/*.json
var baselineFS embed.FS

// SearchSpaceStrategies is the registered set, in the fixed joint-sampling
// order (search_spaces.py:SEARCH_SPACES + suggest_joint_params). The ORDER
// matters: suggest_joint_params samples sepa -> sector_rotation -> pairs, which
// determines optimizer RNG consumption order.
var SearchSpaceStrategies = []string{"sepa", "sector_rotation", "pairs"}

func isRegistered(strategy string) bool {
	for _, s := range SearchSpaceStrategies {
		if s == strategy {
			return true
		}
	}
	return false
}

// LoadBaselineParams parses the embedded baseline JSON for strategy. Any of the
// four baseline files (incl. intraday_breakout) can be loaded; registration is
// a separate concern enforced by SuggestParams/SuggestJointParams.
func LoadBaselineParams(strategy string) (*StrategyParams, error) {
	raw, err := baselineFS.ReadFile("baseline/" + strategy + ".json")
	if err != nil {
		return nil, fmt.Errorf("strategy params file not found in env dir nor baseline: %s.json", strategy)
	}
	return ParseStrategyParams(raw, strategy)
}

// SuggestParams samples one registered strategy's params (search_spaces.py
// suggest_params); unknown -> ValueError("unknown strategy: <name>").
func SuggestParams(strategy string, trial TrialLike) (map[string]float64, error) {
	if !isRegistered(strategy) {
		return nil, fmt.Errorf("unknown strategy: %s", strategy)
	}
	sp, err := LoadBaselineParams(strategy)
	if err != nil {
		return nil, err
	}
	return SuggestWith(sp, trial)
}

// SuggestJointParams samples all three registered spaces from one trial in the
// fixed order sepa -> sector_rotation -> pairs (search_spaces.py
// suggest_joint_params), returning the nested map keyed by strategy.
func SuggestJointParams(trial TrialLike) (map[string]map[string]float64, error) {
	out := make(map[string]map[string]float64, len(SearchSpaceStrategies))
	for _, s := range SearchSpaceStrategies {
		m, err := SuggestParams(s, trial)
		if err != nil {
			return nil, err
		}
		out[s] = m
	}
	return out, nil
}

// BaselineStrategies lists every embedded baseline file stem (sorted), for
// promotion/loader fallback enumeration.
func BaselineStrategies() []string {
	entries, _ := baselineFS.ReadDir("baseline")
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if len(name) > 5 && name[len(name)-5:] == ".json" {
			out = append(out, name[:len(name)-5])
		}
	}
	sort.Strings(out)
	return out
}
