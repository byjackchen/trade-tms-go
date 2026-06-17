// Package metrics implements the backtest performance metrics, specified in
// docs/spec/hyperopt-metrics.md §1.
//
// All functions are pure over an equity curve ([]float64). The mean and
// population standard deviation are computed in exact rational arithmetic so
// the result is bit-for-bit reproducible across platforms (arm64 vs x86),
// independent of compiler instruction selection (spec §1.3). The numeric
// baselines are pinned by the golden vectors in metrics_test.go.
//
// Conventions (spec §1.1):
//   - periods_per_year = 252, no risk-free rate, no annualization overrides.
//   - max_drawdown_pct is a NON-POSITIVE percent (e.g. -10.0 means -10%).
//   - calmar uses geometric annualization with a synthetic 1% drawdown floor
//     for zero-drawdown positive-growth curves.
//
// The BacktestMetrics struct defines the field set and JSON keys
// (spec §1.1); the counters (num_orders, num_filled_orders,
// num_rejected_orders, num_positions) are supplied by the caller (the engine /
// run assembler) because they are not derivable from the equity curve.
package metrics
