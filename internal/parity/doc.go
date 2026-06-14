// Package parity is the golden-parity infrastructure: it loads the canonical
// order SCRIPT and the wrangled bar inputs shared with the reference Nautilus
// harness, drives the Go engine over them with the zero-cost nautilus-compat
// fill profile, and emits the legacy runs/<ts>/*.json artifact set for
// field-by-field diffing against the Nautilus dump.
//
// The whole point is determinism: the same script + the same bars always
// produce the same Go output, and that output must match Nautilus exactly
// (prices after fixed-point, pnl/equity within a cent, counts and ordering
// exact). See docs/spec/engine-fill-model.md for the discovered fill rule the
// nautilus-compat profile reproduces.
//
// Layer: test/parity harness (above the engine, outside the live path). It
// drives the engine and serializes artifacts; it makes no trading decisions of
// its own and is consumed only by parity tests and the comparator tooling.
//
// May import: internal/domain, internal/engine, internal/runs,
// internal/data/calendar, and the standard library. It must NOT import runner,
// livengine, publish or any adapter package.
package parity
