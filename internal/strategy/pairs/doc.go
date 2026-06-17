// Package pairs is the Pairs SignalGenerator — statistical mean-reversion on
// the price spread of two co-moving equities.
//
// Numerical contract (docs/spec/strategy-pairs.md):
//
//   - OLS hedge ratio, population (ddof=0) spread std, strict z-score
//     thresholds, floor sizing, and the FLAT/LONG_SPREAD/SHORT_SPREAD state
//     machine. All float math is float64; the spread/z window INCLUDES the
//     current bar's close (signal at the close of bar t; §7.3).
//   - Multi-symbol bar synchronization: a pair is evaluated only when BOTH
//     legs carry a bar at the current bar's UTC date (the look-ahead guard,
//     §6.2). Bookkeeping (history/last_close/last_bar_date) advances
//     unconditionally before any sync check.
//   - on_bar(Bar) -> []Signal; a pair entry emits 2 signals atomically
//     (long_leg first, short_leg second); a close emits one FLAT per non-zero
//     leg. evaluate_intent and state_summary are read-side observability that
//     never affect trading.
//
// The generator plugs into the internal/core deterministic event loop via the
// engine Strategy seam through an adapter (adapter.go): on_bar emits Signals,
// which the adapter translates to market orders (LONG->BUY, SHORT->SELL,
// FLAT->close net position).
package pairs
