// Package core provides cross-cutting primitives that sit just above
// domain: event types and the event bus contract, clock abstraction
// (backtest simulated clock vs wall clock), trading-calendar helpers and
// shared error kinds — the glue shared by the runner and portfolio layers.
//
// Rules:
//   - May import internal/domain only.
//   - No direct database / network access; those live in adapters and db.
package core
