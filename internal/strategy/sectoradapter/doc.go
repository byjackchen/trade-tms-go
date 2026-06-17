// Package sectoradapter bridges the PURE SectorRotation SignalGenerator
// (internal/strategy/sectorrotation) to the engine-facing Strategy seam
// (internal/engine), translating the SG's multi-symbol rebalance signals into
// market orders (strategy-sector-orb.md):
//
//   - LONG -> BUY of signal.target_qty (the SG only emits LONG for symbols not
//     currently held, so target_qty is the full target position).
//   - FLAT -> close the entire live net position (SELL if long, BUY if short);
//     a flat book is a no-op.
//
// This package — NOT the pure sectorrotation package — is the only place that
// imports engine, preserving the two-layer constraint: the core strategy
// package never imports broker/engine code. It also implements the
// P3 capability seams (SignalEvaluator, StateSummarizer, StatePersister) the
// engine probes by type assertion. SectorRotation consumes no per-bar context,
// so ContextConsumer is intentionally NOT implemented.
//
// Layer: strategy adapter (the engine bridge). Sits between the pure strategy
// package and the engine; the sole importer of internal/engine on the
// SectorRotation path.
//
// May import: internal/domain, internal/engine, internal/strategy/sectorrotation,
// and the standard library. It must NOT import runner, livengine, publish or
// sibling adapter packages.
package sectoradapter
