// Package domain holds the core value types shared across the whole system:
// instruments/symbols, bars (OHLCV), signals, orders, fills, positions and
// account snapshots, as explicit, dependency-free Go types.
//
// Rules:
//   - No I/O, no logging, no third-party deps. Pure data + invariants.
//   - Every other internal package may import domain; domain imports nothing
//     from internal/.
//
// Numeric model (docs/spec/domain-types-money.md §1):
//   - Price and Money are int64 fixed point at 1e-4 scale, with
//     overflow-checked arithmetic, exact string parsing, and JSON encodings
//     that round-trip exactly (see fixed.go and money.go).
//   - Qty is a whole signed share count (positive long, negative short).
//   - float64 conversions round HALF-TO-EVEN (ROUND_HALF_EVEN);
//     PyRound/PyRound4 round a float to n decimal places, ties-to-even.
//   - Strategy-internal indicator math stays float64; the fixed point is for
//     prices, money and the risk pipeline only.
package domain
