package pairs

// build.go: equity-provider construction helpers for the pure pairs Generator.
//
// The resolved-params -> Generator translation deliberately lives in the live
// assembler (strategyassembly.buildPairs), NOT here: keeping this pure-compute
// package free of any internal/params dependency preserves the "pure strategy
// imports only domain/indicators" symmetry and keeps the Postgres client (pgx)
// out of the pure-compute / golden dependency closure.

// ConstantEquity returns an EquityProvider that always reports the same equity,
// matching the EOD-refresh path's constant-captured provider.
func ConstantEquity(usd float64) EquityProvider {
	return func() float64 { return usd }
}
