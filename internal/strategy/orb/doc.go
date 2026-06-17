// Package orb is the intraday_breakout (Opening Range Breakout)
// SignalGenerator. The emitted signal sequence — dates, side, sizing,
// stop/target prices, state-machine transitions, intents — including
// NaN/warmup/look-ahead edge cases, is pinned by the embedded golden test
// (golden_test.go vs testdata/orb_golden.json).
//
// Two-layer contract: this package is PURE — bars in, signals out — and imports
// nothing from the execution engine. The engine translation (Signal -> market
// order, FLAT -> net-position close, capability publishing) lives in
// internal/strategy/orbadapter, the only place that imports internal/engine.
//
// Numeric fidelity (spec §4): ORB carries OHLC and stop/target prices through
// scale-propagating decimal arithmetic (see pydec.go) so the reason /
// state_summary / state_dict strings render deterministically (e.g. stop
// "100.980", target "104.0400"); sizing and intent strength/proximity use
// float64 with int(risk_dollar // float(stop_distance)) and float(Decimal/...)
// math.
package orb
