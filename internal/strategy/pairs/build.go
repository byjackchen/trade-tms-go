package pairs

// build.go: construct a Generator from resolved internal/params, bridging the
// centralized parameter scheme (spec §5) to the SG config.

import (
	"github.com/byjackchen/trade-tms-go/internal/params"
)

// FromParams builds a Generator from a resolved PairsParams document and a live
// equity provider. The provider mirrors the Python equity_provider closure
// (live account equity, pulled at sizing time). Validation matches
// PairsParams.Validate / Config.Validate (spec §4.2, §5).
func FromParams(p params.PairsParams, equity EquityProvider) (*Generator, error) {
	pairList := make([]Pair, 0, len(p.Pairs))
	for _, pr := range p.Pairs {
		pairList = append(pairList, Pair{LongLeg: pr.LongLeg, ShortLeg: pr.ShortLeg})
	}
	cfg := Config{
		EquityProvider:    equity,
		Pairs:             pairList,
		Lookback:          int(p.Lookback),
		EntryZ:            p.EntryZ,
		ExitZ:             p.ExitZ,
		CapitalPerPairPct: p.CapitalPerPairPct,
		Timezone:          p.Timezone,
	}
	return New(cfg)
}

// ConstantEquity returns an EquityProvider that always reports the same equity,
// matching the EOD-refresh path's constant-captured provider (refresh.py:339)
// and the test providers (lambda: Decimal("100000")).
func ConstantEquity(usd float64) EquityProvider {
	return func() float64 { return usd }
}
