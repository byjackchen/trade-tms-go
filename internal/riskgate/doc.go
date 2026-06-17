// Package portfolio implements position sizing, risk limits, capital allocation
// across strategies, portfolio-level accounting (equity curve, exposure,
// realized and unrealized PnL) and the glue that merges multiple strategies'
// signals into one account.
//
// Rules:
//   - Deterministic: same inputs produce the same allocations in backtest
//     and live.
//   - Money math uses explicit, documented rounding.
//
// Implemented (per docs/spec/portfolio-risk.md):
//
//   - Pre-trade risk-gating pipeline (§2-§4): Allocator (per-strategy capital
//     budget, 40/30/20 SEPA/Sector/Pairs split + cash slack, gross-exposure
//     budget check, FLAT bypass) and RiskConstraints (daily_loss_halt /
//     max_single_name GROSS per-strategy / concentration NET cross-strategy),
//     composed by Gate with first-rejection-wins ordering and stable rule names.
//     FLAT and qty<=0 bypass every rule including the halt; the halt does NOT
//     bypass FLAT.
//   - PortfolioHealthSnapshot (§4.2-§4.3): pure read of day P&L, halt boolean,
//     headroom and largest-net concentration; ratios use 28-significant-digit
//     ROUND_HALF_EVEN division.
//   - Reconciliation (§6): EOD sum(strategy books) vs broker net per symbol,
//     with the matched / mismatch / one-sided classification and summary text.
//   - Backtest context providers (§7): ComputeRegime (SPY 200d MA + slope),
//     LoadSF1MarketCaps and LoadEarningsCalendar — the regime / fundamentals /
//     earnings data providers — plus the SharedContextState store and a
//     look-ahead-safe ContextProvider that emits RegimeUpdate / MarketCapUpdate /
//     EarningsBlackoutUpdate value types per bar with dedup semantics.
//
// All money comparisons use exact rational arithmetic (decimal.go, math/big.Rat)
// so they are bit-for-bit reproducible across platforms (arm64 vs x86); the
// golden fixtures (testdata/risk_golden.json, health_golden.json,
// context_golden.json) cover every rule and every context edge case (warmup,
// NaN poisoning, as_of look-ahead, datekey ties, blackout boundaries).
//
// Remaining (tracked): the four signal strategies' sizing/on_bar orchestration
// and the engine glue that builds a ProposedOrder + PortfolioSnapshot per signal
// and feeds Gate.Check at submission time.
package riskgate
