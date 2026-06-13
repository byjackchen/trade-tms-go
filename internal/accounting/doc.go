// Package accounting models positions, the margin account, and the per-bar
// equity-curve sampler — the Go port of the position/account semantics the
// Python reference delegates to Nautilus (AccountType.MARGIN, OmsType.NETTING)
// plus EquityCurveSamplerActor.
//
// # Semantics locked from an empirical Nautilus run (zero-fee equity, no
// slippage, leverage 10, margin_init/maint 0):
//
//   - NETTING: exactly one Position per (strategy_id, symbol). Increasing a
//     position re-weights the average entry price; realized PnL stays 0.
//     Reducing or closing realizes closed_qty * (fill_px - avg_entry) for a
//     long (signed; shorts use avg_entry - fill_px). A FLIP (fill quantity
//     exceeds the open quantity and reverses sign) closes the old position
//     fully — realizing its PnL — then opens a new position at the fill price
//     for the residual quantity, with realized PnL reset for the new position.
//
//   - Account: base-currency (USD) balance = starting_balance + cumulative
//     realized PnL across all positions. The zero-margin equity instrument
//     keeps Free == Total (no locked margin in practice). An AccountState is
//     emitted on every fill (one per settlement), carrying the post-settlement
//     Total and the fill ts — matching Nautilus's "one AccountState per
//     balance-mutating event" cadence (the venue additionally emits an initial
//     state at the starting balance; the engine assembler emits that).
//
//   - Equity = cash + Σ unrealized over open positions, where unrealized for a
//     position uses the last seen price: long  qty*(last-avg), short
//     qty*(last-avg) with qty signed. The equity-curve sampler mirrors the
//     Python EquityCurveSamplerActor: per daily bar it aggregates, per
//     strategy, realized PnL plus unrealized for open positions (open positions
//     use last price total_pnl; flat strategies contribute realized only), and
//     a total across strategies.
package accounting
