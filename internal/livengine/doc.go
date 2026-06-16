// Package livengine is the live (real-time) engine path: the same internal/core
// event loop driven by a WallClock (real time) or a VirtualClock (deterministic
// tests), fed by a streaming DataFeed (moomoo Qot_UpdateKL push OR the mock
// OpenD) instead of a pre-loaded bar queue (P5 locked decision 3).
//
// It REUSES the backtest building blocks verbatim — the strategy adapters
// (SEPA / Pairs / SectorRotation / ORB), the portfolio gate, the look-ahead-safe
// context provider and the out-of-band warmup priming — so the live path and the
// batch (backtest / EOD-replay) path evaluate identical strategy state from
// identical bars. The ONLY substitution is execution: a NoopExecutor records a
// SignalIntent per strategy per bar (and emits a PortfolioHealthSnapshot) but
// submits NO orders and mutates NO account (signal mode, decision 3 + 6). There
// are no positions in signal mode, so the daily-loss halt is informational.
//
// # Consistency contract (the accuracy anchor)
//
// Because there is no Python live golden, INTERNAL consistency is the accuracy
// anchor: a streaming run over a VirtualClock must emit SignalIntents IDENTICAL
// to what a batch replay of the same bars produces (consistency_test.go). This
// holds because both paths:
//
//   - prime the SAME WarmupConsumer strategies from the SAME pre-window history;
//   - feed the SAME run-window bars to the SAME strategy.OnBar in the SAME
//     per-timestamp registration order (SPY heartbeat first for context);
//   - inject the SAME per-bar context on the SPY heartbeat;
//   - call EvaluateIntent at the SAME points (after every strategy's OnBar at a
//     timestamp), so the generation counters and intent payloads coincide.
//
// # Modes
//
// Signal mode is the only execution mode wired in P5; paper/live order
// submission (FLATTEN, real fills) is deferred to P6 (decision 1 + 6).
//
// # Build phases
//
// Build1 (this build) defines the core wiring, the NoopExecutor, the publish /
// persist seams (in-memory recorder for tests) and the `tms trade run --mode signal`
// subcommand scaffold. Build2 wires the real DB upsert (live.signal_intents) and
// the Redis stream publisher behind the same seams.
package livengine
