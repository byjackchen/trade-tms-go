package pairs

// pair.go: the Pair value type and the default pair universe (spec §3).

// Pair is a trade pair. The long_leg / short_leg labels are arbitrary
// direction anchors — the strategy trades the spread BOTH ways depending on
// the z-score sign. Convention (doc only): long_leg = larger-cap / more liquid
// name. Equality is positional and case-sensitive.
type Pair struct {
	LongLeg  string
	ShortLeg string
}

// PairKey is the ordered (long_leg, short_leg) tuple used as the map key for
// all per-pair state. Positional, case-sensitive.
type PairKey struct {
	Long  string
	Short string
}

// Key returns the pair's map key.
func (p Pair) Key() PairKey { return PairKey{Long: p.LongLeg, Short: p.ShortLeg} }

// DefaultPairs is the static default universe (spec §3.2):
// KO/PEP (beverage duopoly), MA/V (payment networks), XOM/CVX (oil majors).
// Exactly 3 pairs, in this order. Empirically chosen, not cointegration-vetted.
func DefaultPairs() []Pair {
	return []Pair{
		{LongLeg: "KO", ShortLeg: "PEP"},
		{LongLeg: "MA", ShortLeg: "V"},
		{LongLeg: "XOM", ShortLeg: "CVX"},
	}
}

// PairState is the per-pair state-machine value. Persisted verbatim
// (spec §7.4).
type PairState string

const (
	StateFlat        PairState = "FLAT"
	StateLongSpread  PairState = "LONG_SPREAD"
	StateShortSpread PairState = "SHORT_SPREAD"
)
