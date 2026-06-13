// Package strategy defines the Strategy interface (on-bar signal
// generation over indicator-enriched data) and will host the Go ports of
// every strategy in the Python reference's src/strategies/ — none may be
// dropped or simplified. Parameter loading follows the centralized-params
// scheme: baseline params embedded per strategy, overridable by a tuned
// params dir (TMS_STRATEGY_PARAMS_DIR), per-strategy fallback to baseline.
//
// Rules:
//   - Strategies are pure with respect to I/O: bars in, signals out.
//   - Numerical semantics must match the Python reference exactly
//     (validated against golden outputs produced by the reference repo).
package strategy
