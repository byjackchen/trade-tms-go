// Package pairsadapter bridges the PURE Pairs SignalGenerator
// (internal/strategy/pairs) to the engine-facing Strategy seam
// (internal/engine), translating each emitted Signal into a market order
// exactly as the reference PairsRunner._submit_for_signal
// (strategy-pairs.md §10, nautilus_runner.py):
//
//   - LONG  -> market BUY of target_qty
//   - SHORT -> market SELL of target_qty (margin account)
//   - FLAT  -> close the live net engine position (SELL if long, BUY if short);
//     a flat book is a no-op. FLAT sizes from the broker's ACTUAL net position,
//     NOT from the SG leg_position, so it survives partial fills / manual
//     intervention.
//
// This package — NOT the pure pairs package — is the only place that imports
// engine, preserving the Eng-D2 two-layer constraint: the core strategy
// package never imports broker/engine code. It implements the P3 capability
// seams (IntentEvaluator, StateSummarizer, StatePersister) the engine probes by
// type assertion. Pairs consumes no per-bar context, so ContextConsumer is
// intentionally NOT implemented.
//
// Layer: strategy adapter (the engine bridge). Sits between the pure strategy
// package and the engine; the sole importer of internal/engine on the Pairs
// path.
//
// May import: internal/domain, internal/engine, internal/strategy/pairs, and
// the standard library. It must NOT import runner, livengine, publish or
// sibling adapter packages.
package pairsadapter
