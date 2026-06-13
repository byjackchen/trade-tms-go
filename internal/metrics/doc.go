// Package metrics implements the backtest performance metrics of the Python
// reference (src/research/metrics.py), specified in
// docs/spec/hyperopt-metrics.md §1 [MUST-MATCH].
//
// All functions are pure over an equity curve ([]float64) — IEEE-754 double
// arithmetic, matching Python's statistics.mean / statistics.pstdev / math.sqrt
// to within full float64 precision. Per spec §1.3 [IMPROVE], the mean and
// population standard deviation use Neumaier compensated summation so the Go
// output agrees with CPython's exact-fraction summation for the reference
// vectors (verified to <=1e-12 relative in metrics_test.go golden vectors).
//
// Conventions (spec §1.1):
//   - periods_per_year = 252, no risk-free rate, no annualization overrides.
//   - max_drawdown_pct is a NON-POSITIVE percent (e.g. -10.0 means -10%).
//   - calmar uses geometric annualization with a synthetic 1% drawdown floor
//     for zero-drawdown positive-growth curves.
//
// The BacktestMetrics struct mirrors the reference field set and JSON keys
// exactly (spec §1.1); the counters (num_orders, num_filled_orders,
// num_rejected_orders, num_positions) are supplied by the caller (the engine /
// run assembler) because they are not derivable from the equity curve.
package metrics
