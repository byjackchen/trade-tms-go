// Package orb is the pure Go port of the Python reference's
// intraday_breakout (Opening Range Breakout) SignalGenerator
// (src/strategies/intraday_breakout/signal.py + intent.py). It is a faithful,
// no-simplification replica: the emitted signal sequence — dates, side, sizing,
// stop/target prices, state-machine transitions, intents — matches the pure
// Python signal.py exactly, including NaN/warmup/look-ahead edge cases, proven
// by the embedded golden parity test (parity_test.go vs testdata/orb_parity.json
// dumped from the reference venv).
//
// Two-layer contract (Eng-D2 [MUST-MATCH]): this package is PURE — bars in,
// signals out — and imports nothing from the execution engine. The engine
// translation (Signal -> market order, FLAT -> net-position close, capability
// publishing) lives in internal/strategy/orbadapter, the only place that
// imports internal/engine.
//
// Numeric fidelity (spec §4 [MUST-MATCH]): ORB carries OHLC and stop/target
// prices through CPython-Decimal-faithful scale-propagating arithmetic
// (see pydec.go) so the reason / state_summary / state_dict strings render
// byte-identically (e.g. stop "100.980", target "104.0400"); sizing and
// intent strength/proximity use float64 mirroring the reference's
// int(risk_dollar // float(stop_distance)) and float(Decimal/...) math.
package orb
