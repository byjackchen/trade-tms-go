// Package livetrade wires the paper/live trading session: it composes the
// already-built native moomoo Trd_* client + MoomooExecutor + order state
// machine + mock/real venue into a runnable trading node, adding the safety
// machinery that the unit-level executor does NOT own (P6 locked decisions
// 1, 4, 5, 6, 7):
//
//   - GatedSubmitter (decision 1+4): the PRE-SUBMIT portfolio gate. Every
//     OPENING order passes Portfolio.Check (allocator budget + aggregate risk
//     constraints) AND the daily-loss-halt rule BEFORE the MoomooExecutor calls
//     PlaceOrder. A rejection records a live.risk_events row + audit and the
//     order is suppressed (never sent). FLAT / closing orders bypass the budget
//     and the halt (closes always proceed) per docs/spec/portfolio-risk.md.
//
//   - AccountAdapter: a thin moomoo.AccountBook over accounting.Account, so the
//     executor settles broker fills into the SAME accounting code the backtest
//     uses (NETTING positions, realized PnL). The strategy books read back
//     through it for FLAT close sizing and reconciliation.
//
//   - Reconciler (decision 5): compares broker GetPositionList against the
//     strategy books and writes a live.reconciliation_reports row. On a mismatch
//     it ALERTS (halt + cockpit surface) but NEVER auto-trades to correct.
//
//   - Recovery (decision 6): on startup, restore each strategy's SG state_dict
//     from PG, restore positions from the broker (RestoreFromBroker), and run a
//     reconciliation so the node resumes cleanly after a crash.
//
//   - Flatten (decision 7): the kill-switch close-all. A confirmation-gated,
//     idempotent set of FLAT market orders closing every open broker position.
//
// SAFETY is the top acceptance criterion (this code can place real orders in the
// future): the live-activation gate (typed phrase + real acc id + UnlockTrade +
// the TMS-LIVE-REAL-001 trader-id namespace) lives in the MoomooExecutor
// constructor and is re-asserted on every submission; this package never
// constructs a live executor without it. signal/paper can never reach the live
// account.
package livetrade
