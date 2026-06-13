package runs

// storehelpers.go provides the deterministic per-strategy map-key orderings
// store.go uses when persisting metrics and equity curves: row insertion order
// must be stable across runs (a prerequisite for byte-identical re-runs and
// reproducible fixtures). They wrap the generic SortedKeys helper (pyjson.go).

import "github.com/byjackchen/trade-tms-go/internal/metrics"

// sortedMetricKeys returns the strategy ids of a per-strategy metrics map in
// ascending order.
func sortedMetricKeys(m map[string]metrics.BacktestMetrics) []string {
	return SortedKeys(m)
}

// sortedEquityKeys returns the strategy ids of a per-strategy equity map in
// ascending order.
func sortedEquityKeys(m map[string][]EquityPoint) []string {
	return SortedKeys(m)
}
