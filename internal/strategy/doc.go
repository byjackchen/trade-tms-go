// Package strategy defines the Strategy interface (on-bar signal
// generation over indicator-enriched data) and hosts the strategy
// implementations. Parameter loading follows the centralized-params
// scheme: baseline params embedded per strategy, overridable by a tuned
// params dir (TMS_STRATEGY_PARAMS_DIR), per-strategy fallback to baseline.
//
// Rules:
//   - Strategies are pure with respect to I/O: bars in, signals out.
//   - Numerical semantics are pinned by golden tests.
package strategy
