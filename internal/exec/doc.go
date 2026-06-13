// Package exec defines the execution layer: the ExecutionClient interface
// plus implementations — simulated fills for backtests (slippage,
// commission, next-bar fill model identical to the Python reference) and,
// in later phases, the moomoo OpenD paper/live client. Mirrors the exec
// responsibilities embedded in src/runner/ and src/adapters of the
// reference repo.
//
// Rules:
//   - Backtest and live clients satisfy the same interface; the engine
//     cannot tell them apart.
//   - Live order paths must be idempotent and audit-logged.
package exec
