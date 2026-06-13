// Package portfolio is the Go port of the Python reference's
// src/portfolio/: position sizing, risk limits, capital allocation across
// strategies, portfolio-level accounting (equity curve, exposure, realized
// and unrealized PnL) and the glue that merges multiple strategies' signals
// into one account (src/runner/portfolio_glue.py).
//
// Rules:
//   - Deterministic: same inputs produce the same allocations in backtest
//     and live.
//   - Money math uses explicit, documented rounding identical to the
//     reference implementation.
//
// Implemented (P2 slice): the pre-trade risk-gating pipeline (spec
// domain-types-money §5, [MUST-MATCH]) — Allocator (per-strategy capital
// budget) and RiskConstraints (daily_loss_halt / max_single_name /
// concentration), composed by Portfolio with first-rejection-wins ordering and
// the exact reference rule names. All comparisons use exact rational arithmetic
// (decimal.go, math/big.Rat) so they reproduce CPython decimal.Decimal results
// bit-for-bit; a 400-case cross-language parity fixture covers every rule. This
// is the gate that makes num_rejected_orders a real, non-zero metric.
//
// Remaining (tracked): the four signal strategies' sizing/on_bar orchestration
// (§4/§6) and the engine glue that builds a ProposedOrder + AccountSnapshot per
// signal and feeds Portfolio.Check at submission time.
package portfolio
