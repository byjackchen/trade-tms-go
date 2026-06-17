// Package runner is the outermost composition layer: it assembles and drives
// the live trading node and the EOD (end-of-day replay) node, wiring the
// concrete adapters (moomoo OpenD client or mock), the streaming/replay feed,
// the livengine.Session, the publish sink (Postgres signal_intents + Redis
// streams) and the ops.commands control plane into a running supervisor. It is
// the Go counterpart of the Python reference's live_runner.py / EOD scripts and
// holds no strategy or numerical logic of its own — only orchestration,
// lifecycle (ctx-cancellation, graceful drain, no goroutine leaks) and
// DB-driven strategy-set resolution that reuses the SAME assembly the backtest
// path uses (P5 decision 3).
//
// Layer: composition root (outermost). Everything depends on runner only via
// main; runner depends inward on the engine, strategy, data, exec and
// persistence layers.
//
// May import: internal/domain, internal/core, internal/params,
// internal/riskgate, internal/accounting, internal/engine,
// internal/engine/strategyassembly, internal/livengine, internal/livetrade,
// internal/commands, internal/publish, internal/data/calendar,
// internal/data/universe, internal/adapters/moomoo (+ its pb/* messages),
// internal/exec/moomoo, and the standard library / pgx. It is imported only by
// cmd/tms (and tests); no inner package may import runner.
package runner
