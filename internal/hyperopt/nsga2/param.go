package nsga2

// param.go defines the parameter-encoding model the optimizer searches over.
//
// The encoding follows standard NSGA-II search-space semantics
// (docs/spec/hyperopt-metrics.md §2.3, §6.4): each parameter is a bounded
// numeric distribution — float (uniform) or int (uniform inclusive, step 1) —
// plus an optional categorical axis. Log scaling is supported for completeness
// (a float/int distribution carries a `log` flag and the NSGA-II uniform
// crossover operates in the transformed internal space). The shipped baseline
// spaces (sepa/sector_rotation/pairs) are all linear; log is exercised by the
// synthetic correctness tests only.
//
// Why an internal/external split: Optuna's UniformCrossover swaps parameter
// values in the *internal* representation produced by _SearchSpaceTransform —
// linear params map identity-ish, log params map through log10, and ints map to
// a continuous internal axis that is rounded on untransform. Modelling the
// internal axis explicitly keeps crossover/mutation type-correct and bounded,
// and makes the optimizer a pure function of the seeded PRNG.

import (
	"fmt"
	"math"
)

// ParamKind enumerates the supported parameter encodings.
type ParamKind int

const (
	// KindFloat is a continuous uniform parameter in [Low, High].
	KindFloat ParamKind = iota
	// KindInt is an integer uniform parameter, inclusive of both ends, step 1.
	KindInt
	// KindCategorical is an unordered choice over Choices.
	KindCategorical
)

// Param describes one searchable dimension.
//
// For KindFloat/KindInt the bounds Low<=High are required. Log==true selects
// log-space sampling/crossover (requires Low>0). For KindCategorical, Choices
// holds the candidate values (compared by ==); Low/High/Log are ignored.
type Param struct {
	Name    string
	Kind    ParamKind
	Low     float64
	High    float64
	Log     bool
	Choices []any
}

// FloatParam constructs a uniform continuous parameter.
func FloatParam(name string, low, high float64) Param {
	return Param{Name: name, Kind: KindFloat, Low: low, High: high}
}

// LogFloatParam constructs a log-uniform continuous parameter (low>0).
func LogFloatParam(name string, low, high float64) Param {
	return Param{Name: name, Kind: KindFloat, Low: low, High: high, Log: true}
}

// IntParam constructs a uniform integer parameter, inclusive of both ends.
func IntParam(name string, low, high int64) Param {
	return Param{Name: name, Kind: KindInt, Low: float64(low), High: float64(high)}
}

// CategoricalParam constructs an unordered categorical parameter.
func CategoricalParam(name string, choices ...any) Param {
	return Param{Name: name, Kind: KindCategorical, Choices: choices}
}

// validate checks bound/log/choice sanity for one parameter.
func (p Param) validate() error {
	switch p.Kind {
	case KindFloat, KindInt:
		if !(p.Low <= p.High) || math.IsNaN(p.Low) || math.IsNaN(p.High) ||
			math.IsInf(p.Low, 0) || math.IsInf(p.High, 0) {
			return fmt.Errorf("param %q: invalid bounds [%g, %g]", p.Name, p.Low, p.High)
		}
		if p.Log && p.Low <= 0 {
			return fmt.Errorf("param %q: log scale requires low>0 (got %g)", p.Name, p.Low)
		}
	case KindCategorical:
		if len(p.Choices) == 0 {
			return fmt.Errorf("param %q: categorical requires >=1 choice", p.Name)
		}
	default:
		return fmt.Errorf("param %q: unknown kind %d", p.Name, p.Kind)
	}
	return nil
}

// SearchSpace is an ordered list of parameters. Order is significant: it fixes
// the per-individual gene order and therefore the PRNG consumption order, which
// makes a given seed reproduce an identical population trajectory.
type SearchSpace struct {
	Params []Param
	index  map[string]int
}

// NewSearchSpace validates the parameters and builds the name index.
// Duplicate names are rejected.
func NewSearchSpace(params ...Param) (*SearchSpace, error) {
	if len(params) == 0 {
		return nil, fmt.Errorf("search space must have >=1 parameter")
	}
	idx := make(map[string]int, len(params))
	for i, p := range params {
		if p.Name == "" {
			return nil, fmt.Errorf("param at position %d has empty name", i)
		}
		if _, dup := idx[p.Name]; dup {
			return nil, fmt.Errorf("duplicate param name %q", p.Name)
		}
		if err := p.validate(); err != nil {
			return nil, err
		}
		idx[p.Name] = i
	}
	// Copy to defend against later caller mutation of the slice.
	cp := make([]Param, len(params))
	copy(cp, params)
	return &SearchSpace{Params: cp, index: idx}, nil
}

// Len reports the number of dimensions.
func (s *SearchSpace) Len() int { return len(s.Params) }

// Index returns the position of name, or -1.
func (s *SearchSpace) Index(name string) int {
	if i, ok := s.index[name]; ok {
		return i
	}
	return -1
}

// Genome is the internal representation of one individual: one float per
// parameter, in SearchSpace order.
//
//   - KindFloat (linear): the value itself.
//   - KindFloat (log):    log10(value).
//   - KindInt:            the integer value as a float (already rounded).
//   - KindCategorical:    the chosen index into Choices, as a float.
//
// Keeping a single []float64 lets uniform crossover (per-gene swap) and the
// drop-and-resample mutation operate uniformly across kinds.
type Genome []float64

// Clone returns an independent copy of the genome.
func (g Genome) Clone() Genome {
	cp := make(Genome, len(g))
	copy(cp, g)
	return cp
}

// Params is the externally meaningful, decoded parameter map for an individual.
type Params map[string]any

// sampleGene draws a uniform value for parameter i in internal coordinates.
func (s *SearchSpace) sampleGene(i int, r *rng) float64 {
	p := s.Params[i]
	switch p.Kind {
	case KindFloat:
		if p.Log {
			lo, hi := math.Log10(p.Low), math.Log10(p.High)
			return lo + r.Float64()*(hi-lo)
		}
		return p.Low + r.Float64()*(p.High-p.Low)
	case KindInt:
		// Inclusive of both ends, step 1: draw an integer in [low, high].
		n := int64(p.High) - int64(p.Low) + 1
		return float64(int64(p.Low) + r.Int64n(n))
	case KindCategorical:
		return float64(r.Intn(len(p.Choices)))
	}
	return 0
}

// sample draws a fresh uniformly-random genome over the whole space.
func (s *SearchSpace) sample(r *rng) Genome {
	g := make(Genome, len(s.Params))
	for i := range s.Params {
		g[i] = s.sampleGene(i, r)
	}
	return g
}

// contains reports whether every gene lies within its parameter's internal
// bounds. Mirrors Optuna's _is_contained check that gates the crossover retry
// loop. For categoricals the gene must be a valid in-range index.
func (s *SearchSpace) contains(g Genome) bool {
	if len(g) != len(s.Params) {
		return false
	}
	for i, p := range s.Params {
		v := g[i]
		if math.IsNaN(v) {
			return false
		}
		switch p.Kind {
		case KindFloat:
			lo, hi := p.Low, p.High
			if p.Log {
				lo, hi = math.Log10(p.Low), math.Log10(p.High)
			}
			if v < lo || v > hi {
				return false
			}
		case KindInt:
			iv := math.RoundToEven(v)
			if iv < p.Low || iv > p.High {
				return false
			}
		case KindCategorical:
			if v < 0 || int(v) >= len(p.Choices) {
				return false
			}
		}
	}
	return true
}

// decode converts an internal genome to the external Params map. Int and
// categorical genes are rounded/indexed; log floats are exponentiated back.
func (s *SearchSpace) decode(g Genome) Params {
	out := make(Params, len(s.Params))
	for i, p := range s.Params {
		switch p.Kind {
		case KindFloat:
			if p.Log {
				out[p.Name] = math.Pow(10, g[i])
			} else {
				out[p.Name] = g[i]
			}
		case KindInt:
			out[p.Name] = int64(math.RoundToEven(g[i]))
		case KindCategorical:
			idx := int(math.RoundToEven(g[i]))
			if idx < 0 {
				idx = 0
			}
			if idx >= len(p.Choices) {
				idx = len(p.Choices) - 1
			}
			out[p.Name] = p.Choices[idx]
		}
	}
	return out
}

// ObjectiveSpec declares one objective's optimization direction.
type ObjectiveSpec struct {
	Name     string
	Maximize bool
}
