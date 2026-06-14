// Package orbadapter bridges the PURE ORB (intraday_breakout) SignalGenerator
// (internal/strategy/orb) to the engine-facing Strategy seam (internal/engine),
// translating the SG's signals into market orders exactly as the reference
// IntradayBreakoutRunner does (strategy-sector-orb.md §3.10, nautilus_runner.py):
//
//   - LONG -> BUY of signal.target_qty, TimeInForce DAY (intraday-only; the
//     engine's market submit is the day-scoped equivalent). The SG sizes the
//     full target position.
//   - FLAT -> close the entire LIVE net position (SELL if long, BUY if short),
//     read from portfolio.net_position; a flat book is a no-op. The SG's carried
//     FLAT qty is NOT used for sizing (it is only the SG's internal held count,
//     surfaced in logs).
//
// This package — NOT the pure orb package — is the only place that imports
// engine, preserving the Eng-D2 two-layer constraint: the core strategy package
// never imports broker/engine code (the AST test on the Python side asserts the
// same). It implements the P3 capability seams (IntentEvaluator,
// StateSummarizer, StatePersister). ORB consumes no per-bar portfolio context
// (no regime/market-cap/earnings — nautilus_runner.py:_runner_ticker reserves
// per-ticker routing but subscribes to none), so ContextConsumer is
// intentionally NOT implemented.
//
// Layer: strategy adapter (the engine bridge). Sits between the pure strategy
// package and the engine; the sole importer of internal/engine on the ORB path.
//
// May import: internal/domain, internal/engine, internal/strategy/orb, and the
// standard library. It must NOT import runner, livengine, publish or sibling
// adapter packages.
package orbadapter
