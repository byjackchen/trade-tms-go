// Package sepa is the Minervini SEPA SignalGenerator. It is the stateful,
// streaming daily long-only state machine: a bar arrives via OnBar, external
// context (regime / market-cap / earnings / catalyst) arrives via setters, and
// the flat-book entry chain (200-bar warmup -> Stage 2 -> Trend Template 8/8 ->
// VCP base on history EXCLUDING the current bar -> strict pivot breakout ->
// 60-bar breakout-volume gate -> grade gate -> grade-aware first-tranche
// sizing) or the held-book hard-stop exit runs.
//
// Layering (spec §0): this package imports ONLY internal/indicators (the
// numerical foundation) and the stdlib — never engine, broker, or portfolio
// packages. The engine bridge lives in internal/strategy/sepaadapter, which is
// the sole importer of engine/domain.
//
// Numerical fidelity: all math is float64 (the indicator layer encodes ddof,
// NaN propagation, rolling min_periods==window, and round-half-even); the
// price-state fields carry both their float64 value and the canonical
// str(Decimal) string (pyFloatRepr) so state_summary / state_dict / intent
// render deterministic strings. The look-ahead asymmetry of locked decision 1 —
// evaluate_intent runs VCP on the FULL klines (current bar INCLUDED) while the
// entry path EXCLUDES the current bar — is documented at both call sites.
//
// Numerical behavior is pinned by golden_test.go against testdata/sepa_golden.json
// over 14 fixed scenarios (entry, every rejection branch, exit, hold,
// equity-scaling), diffed signal-by-signal: dates, side, qty/sizing,
// stop/pivot/entry strings, state-machine states, intent fields, and per-bar
// state_summary.
package sepa
