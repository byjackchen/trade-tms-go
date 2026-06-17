// Package moomoo (internal/exec/moomoo) is the live/paper order-execution
// adapter: the MoomooExecutor that implements the engine's order-submission
// seam (engine.OrderSubmitter + engine.PositionReader) on top of the native
// moomoo Trd_* trading client (internal/broker/moomoo.TradeClient),
// replacing the signal-mode NoopExecutor for paper and live trading.
//
// Components:
//
//   - MoomooExecutor (executor.go / executor_submit.go): builds domain orders
//     from strategy intents, submits them idempotently via the TradeClient
//     (client-order-id dedupe), and drives the order state machine from
//     Trd_UpdateOrder / Trd_UpdateOrderFill pushes — settling fills into
//     accounting, persisting live.orders/fills/positions, and feeding the
//     engine fill sink.
//
//   - Order state machine (statemachine.go): a PURE, deterministic transition
//     from moomoo OrderStatus pushes to domain order-state + per-fill Fill
//     events. Cumulative dealtQty/dealtAvgPrice are converted to per-fill
//     deltas; duplicate pushes are no-ops; terminal states are sticky. The
//     status->event mapping follows the Trd_Common.OrderStatus enum.
//
//   - Live-activation safety (executor.go): the constructor REFUSES to bind the
//     real account unless ALL of {typed confirmation phrase, explicit real acc
//     id, UnlockTrade success, TMS-LIVE-REAL-001 trader id} hold. A paper
//     executor is bound to TrdEnvSimulate and can never name the real account;
//     assertEnvInvariants re-checks on every submission. There is NO code path
//     that submits a non-paper order without the full gate (proven by
//     safety_test.go).
//
//   - Flatten-on-kill + crash recovery (executor_recovery.go): a confirmation-
//     gated Flatten closes all open broker positions; RestoreFromBroker rebuilds
//     in-flight order state (incl. the cumulative-fill snapshot) after a restart
//     so later pushes apply correct deltas, and re-keys each restored order to
//     its ORIGINATING strategy via the StrategyResolver (the strategy id persisted
//     at submit in live.orders) so post-resume fills attribute correctly — never
//     to an empty-strategy orphan. An unresolved order is reported (recovery fails
//     loud) and its fills are rejected by Fill.Validate, not mis-attributed.
//
//   - MockVenue (venue.go): a permanent, in-repo, controllable TradeClient that
//     simulates accept/fill/partial/reject/cancel + positions/funds — the
//     deterministic gate driver. Green-on-mock is built to predict green-on-real
//     because the mock speaks the identical normalised TradeClient surface and
//     cumulative-fill semantics.
package moomoo
