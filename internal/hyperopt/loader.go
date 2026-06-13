package hyperopt

// loader.go ports src/strategies/params/loader.py (spec §2 [MUST-MATCH]):
// parse a strategy param JSON into a StrategyParams, validate it, and provide
// defaults_dict / suggest_with. Parameter INSERTION ORDER of the JSON object is
// preserved — it determines suggest order (and hence the optimizer RNG
// consumption order) and output file order. Go's map iteration is random, so
// parameters are decoded into an ordered slice via json.Decoder token streaming.

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// allowed sets (loader.py:21-25).
var (
	schemaVersionAllowed   = map[int]bool{1: true}
	typesAllowed           = map[string]bool{"float": true, "int": true, "str": true, "list": true}
	numericTypes           = map[string]bool{"float": true, "int": true}
	constraintKindsAllowed = map[string]bool{"clamp_high": true, "clamp_low": true}
)

// SearchSpec is a numeric search range (loader.py:28-31).
type SearchSpec struct {
	Low  float64
	High float64
}

// ParamSpec is one parameter's spec (loader.py:34-41). Default is the raw JSON
// value (json.RawMessage) so list/str/number defaults round-trip verbatim for
// write_tuned_params; DefaultValue decodes it on demand.
type ParamSpec struct {
	Name        string
	Default     json.RawMessage
	Type        string
	Search      *SearchSpec
	Description *string
}

// Constraint is one clamp constraint (loader.py:44-48).
type Constraint struct {
	Kind       string
	Param      string
	Expression string
}

// StrategyParams is the parsed, validated param file (loader.py:51-58).
// Parameters preserves file order; ParamIndex maps name -> position.
type StrategyParams struct {
	Strategy      string
	SchemaVersion int
	Metadata      map[string]json.RawMessage
	Parameters    []ParamSpec
	ParamIndex    map[string]int
	Constraints   []Constraint
}

// Param returns the ParamSpec for name and whether it exists.
func (sp StrategyParams) Param(name string) (ParamSpec, bool) {
	if i, ok := sp.ParamIndex[name]; ok {
		return sp.Parameters[i], true
	}
	return ParamSpec{}, false
}

// ParseStrategyParams parses raw JSON for the requested strategy (loader.py:
// 130-185). Errors mirror the reference ValueError messages exactly.
func ParseStrategyParams(raw []byte, strategy string) (*StrategyParams, error) {
	// Top-level object decode that captures field presence and raw parameters.
	var top struct {
		Strategy      *string                    `json:"strategy"`
		SchemaVersion *int                       `json:"schema_version"`
		Metadata      map[string]json.RawMessage `json:"metadata"`
		Parameters    json.RawMessage            `json:"parameters"`
		Constraints   []struct {
			Kind       *string `json:"kind"`
			Param      string  `json:"param"`
			Expression string  `json:"expression"`
		} `json:"constraints"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, fmt.Errorf("invalid params JSON: %w", err)
	}
	if top.Strategy == nil {
		return nil, fmt.Errorf("missing required field: strategy")
	}
	if *top.Strategy != strategy {
		return nil, fmt.Errorf("file declared strategy '%s' but loader was asked for '%s'", *top.Strategy, strategy)
	}
	if top.SchemaVersion == nil || !schemaVersionAllowed[*top.SchemaVersion] {
		sv := "None"
		if top.SchemaVersion != nil {
			sv = fmt.Sprintf("%d", *top.SchemaVersion)
		}
		return nil, fmt.Errorf("unsupported schema_version %s", sv)
	}

	params, index, err := parseParametersOrdered(top.Parameters)
	if err != nil {
		return nil, err
	}

	constraints := make([]Constraint, 0, len(top.Constraints))
	for _, c := range top.Constraints {
		if c.Kind == nil || !constraintKindsAllowed[*c.Kind] {
			k := "None"
			if c.Kind != nil {
				k = "'" + *c.Kind + "'"
			}
			return nil, fmt.Errorf("constraint kind %s not in {'clamp_high', 'clamp_low'}", k)
		}
		constraints = append(constraints, Constraint{Kind: *c.Kind, Param: c.Param, Expression: c.Expression})
	}

	meta := top.Metadata
	if meta == nil {
		meta = map[string]json.RawMessage{}
	}
	return &StrategyParams{
		Strategy:      strategy,
		SchemaVersion: *top.SchemaVersion,
		Metadata:      meta,
		Parameters:    params,
		ParamIndex:    index,
		Constraints:   constraints,
	}, nil
}

// parseParametersOrdered decodes the "parameters" object preserving key order
// via a streaming token decoder (loader.py:_parse_param, :160-185).
func parseParametersOrdered(rawParams json.RawMessage) ([]ParamSpec, map[string]int, error) {
	if len(bytes.TrimSpace(rawParams)) == 0 || string(bytes.TrimSpace(rawParams)) == "null" {
		return nil, map[string]int{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(rawParams))
	tok, err := dec.Token()
	if err != nil {
		return nil, nil, fmt.Errorf("parameters: %w", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, nil, fmt.Errorf("parameters must be an object")
	}
	var specs []ParamSpec
	index := map[string]int{}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, nil, err
		}
		name := keyTok.(string)
		var body struct {
			Default     json.RawMessage `json:"default"`
			Type        *string         `json:"type"`
			Search      json.RawMessage `json:"search"`
			Description *string         `json:"description"`
			HasDefault  bool            `json:"-"`
		}
		if err := dec.Decode(&body); err != nil {
			return nil, nil, fmt.Errorf("parameter %q: %w", name, err)
		}
		if body.Type == nil || !typesAllowed[*body.Type] {
			tv := "None"
			if body.Type != nil {
				tv = "'" + *body.Type + "'"
			}
			return nil, nil, fmt.Errorf("parameter '%s': type %s not in {'float', 'int', 'list', 'str'}", name, tv)
		}
		var search *SearchSpec
		if st := bytes.TrimSpace(body.Search); len(st) > 0 && string(st) != "null" {
			if !numericTypes[*body.Type] {
				return nil, nil, fmt.Errorf("parameter '%s': search not supported on type '%s'", name, *body.Type)
			}
			var sr struct {
				Low  float64 `json:"low"`
				High float64 `json:"high"`
			}
			if err := json.Unmarshal(body.Search, &sr); err != nil {
				return nil, nil, fmt.Errorf("parameter '%s': bad search: %w", name, err)
			}
			search = &SearchSpec{Low: sr.Low, High: sr.High}
		}
		if len(bytes.TrimSpace(body.Default)) == 0 {
			return nil, nil, fmt.Errorf("parameter '%s': missing required field: default", name)
		}
		index[name] = len(specs)
		specs = append(specs, ParamSpec{
			Name:        name,
			Default:     append(json.RawMessage(nil), body.Default...),
			Type:        *body.Type,
			Search:      search,
			Description: body.Description,
		})
	}
	// consume closing '}'
	if _, err := dec.Token(); err != nil {
		return nil, nil, err
	}
	return specs, index, nil
}

// TrialLike is the suggest interface (loader.py:67-69). suggest_float/int names
// are the PREFIXED "<strategy>.<param>"; the returned map keys are unprefixed.
type TrialLike interface {
	SuggestFloat(name string, low, high float64) float64
	SuggestInt(name string, low, high int64) int64
}

// DefaultsDict returns {name: default} for every parameter, decoded to Go
// values (loader.py:187-189). Numbers decode to float64/int per type, lists to
// []any, strings to string.
func DefaultsDict(sp *StrategyParams) (map[string]any, error) {
	out := make(map[string]any, len(sp.Parameters))
	for _, spec := range sp.Parameters {
		var v any
		if err := json.Unmarshal(spec.Default, &v); err != nil {
			return nil, fmt.Errorf("param %q default: %w", spec.Name, err)
		}
		out[spec.Name] = v
	}
	return out, nil
}

// SuggestWith runs trial.Suggest* per ParamSpec in file order, then applies the
// constraints in file order (loader.py:192-230). Returns ONLY sampled keys
// (those with a search range); static defaults are NOT merged. Constraint
// clamping mutates only the returned map; the trial-recorded value is the raw
// suggestion (the caller's TrialLike records inside Suggest*).
func SuggestWith(sp *StrategyParams, trial TrialLike) (map[string]float64, error) {
	sampled := map[string]float64{}
	for _, spec := range sp.Parameters {
		if spec.Search == nil {
			continue
		}
		full := sp.Strategy + "." + spec.Name
		switch spec.Type {
		case "float":
			sampled[spec.Name] = trial.SuggestFloat(full, spec.Search.Low, spec.Search.High)
		case "int":
			sampled[spec.Name] = float64(trial.SuggestInt(full, int64(spec.Search.Low), int64(spec.Search.High)))
		default:
			return nil, fmt.Errorf("non-numeric param '%s' cannot have a search range", spec.Name)
		}
	}
	for _, c := range sp.Constraints {
		bound, err := safeEval(c.Expression, sampled)
		if err != nil {
			return nil, fmt.Errorf("constraint on '%s': %w", c.Param, err)
		}
		current, ok := sampled[c.Param]
		if !ok {
			return nil, fmt.Errorf("constraint targets unsampled param '%s'", c.Param)
		}
		switch c.Kind {
		case "clamp_high":
			if bound < current {
				sampled[c.Param] = bound
			}
		case "clamp_low":
			if bound > current {
				sampled[c.Param] = bound
			}
		}
	}
	return sampled, nil
}
