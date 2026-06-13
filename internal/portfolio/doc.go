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
package portfolio
