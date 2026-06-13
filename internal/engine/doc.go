// Package engine implements the deterministic event-loop that drives both
// backtests and live trading from the same code path — the Go port of the
// Python reference's src/runner/backtest_runner.py and live_runner.py
// orchestration. It feeds bars to strategies, collects signals, routes them
// through portfolio sizing and on to execution, with identical semantics in
// backtest and live modes (only the data/exec adapters differ).
//
// Rules:
//   - Single-writer event loop; concurrency at the edges (feeds, exec).
//   - Everything context-aware: cancellation stops the loop cleanly.
package engine
