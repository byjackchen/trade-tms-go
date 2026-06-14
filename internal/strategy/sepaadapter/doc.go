// Package sepaadapter bridges the PURE SEPA SignalGenerator
// (internal/strategy/sepa) to the engine-facing Strategy seam
// (internal/engine), translating domain.Bar -> sepa.Bar, injecting per-bar
// portfolio context (regime / market-cap / earnings) via the engine's
// ContextConsumer seam, and translating emitted sepa.Signal target-position
// signals into market orders exactly as the reference NautilusRunner's
// _submit_for_signal (spec §10): LONG -> BUY target_qty; FLAT -> reverse the
// live net position to flat (no-op when already flat); SHORT unsupported.
//
// This package — NOT the pure sepa package — is the only place that imports
// engine/domain, preserving the [MUST-MATCH] layering constraint (spec §0):
// the core SEPA package never imports broker/engine code.
//
// Layer: strategy adapter (the engine bridge). Sits between the pure strategy
// package and the engine; the sole importer of internal/engine on the SEPA
// path. It emits no domain types from the pure layer (E3 invariant).
//
// May import: internal/domain, internal/engine, internal/strategy/sepa, and the
// standard library. It must NOT import runner, livengine, publish or sibling
// adapter packages.
package sepaadapter
