// Package engine implements the deterministic per-bar dispatch that drives
// backtests and live/paper trading from the same shared core — the Go port of
// the Python reference's src/runner/backtest_runner.py and live_runner.py
// orchestration. It feeds bars to strategies, collects signals, and routes them
// through portfolio sizing and on to execution.
//
// # Two consumers, one shared core
//
// There are two consumers of this package's machinery, and they intentionally
// share the parity-sensitive code while running on distinct loop drivers:
//
//   - engine.Engine (this package) is the BATCH driver. It owns its own simulated
//     venue (the concrete *exec.SimExecutor) and the bar-replay loop, including
//     fill-timing (ProcessBar / FlushThisBar / FillAtBar, Model().Timing()) that
//     only the deterministic backtest needs.
//   - livengine.Session is the STREAMING driver. It runs the live/paper order
//     lifecycle, injecting an engine.OrderSubmitter (livengine.NoopExecutor,
//     livetrade.GatedSubmitter or moomoo.MoomooExecutor) rather than hosting
//     SimExecutor, and carries timestamp-rollover / heartbeat guards the batch
//     path does not.
//
// Both consumers share the strategy, portfolio, context-injection and warmup
// code in this package plus the engine.OrderSubmitter / engine.PositionReader
// seam, so strategy behavior is identical across modes. They are deliberately
// NOT merged into one hot-swappable engine: batch fill-timing and the live order
// lifecycle are different loop drivers, and collapsing them would entangle the
// deterministic fill simulation with the live submission path. (See
// docs/architecture.md.)
//
// Rules:
//   - Single-writer event loop; concurrency at the edges (feeds, exec).
//   - Everything context-aware: cancellation stops the loop cleanly.
package engine
