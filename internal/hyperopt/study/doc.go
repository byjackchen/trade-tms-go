// Package study is the hyperopt orchestration + control plane: it runs a
// self-written, deterministic, seeded NSGA-II walk-forward hyper-parameter study
// over the P2/P3 backtest engine (docs/spec/hyperopt-metrics.md §6–§9; P4 locked
// decisions 1–6).
//
// # What it does
//
// Given a strategy (sepa | sector_rotation | pairs | joint), a search space (the
// embedded baseline JSON ranges), the (sharpe, calmar)/(maximize, maximize)
// objective set, a date range, a walk-forward config (anchored-expanding folds +
// embargo, hyperopt §3), population/generations, a seed and a parallelism, the
// Coordinator runs NSGA-II (internal/hyperopt/nsga2) where each candidate's
// objective vector is the AGGREGATE of per-fold backtest metrics. Each fold runs
// the real strategy backtest (internal/engine + strategyassembly) over a SHARED
// READ-ONLY bar dataset loaded ONCE (locked decision 5); per-fold metrics come
// from internal/metrics and are aggregated by concat-and-recompute over the
// stitched return curve (never averaged — hyperopt §4).
//
// # Determinism (locked decisions 1/3/5)
//
// ONE seeded PRNG threads the optimizer. A whole generation is asked up front,
// evaluated by a bounded worker pool of isolated engine instances over the
// immutable dataset, and TOLD BACK IN ASCENDING ID ORDER — so the population
// trajectory and every artifact are independent of trial completion order.
// Re-running the same seed over the same dataset reproduces identical trials.
// Walk-forward splits match the Python splitter exactly; objective values match
// Python exactly for a given param set (the proven P2/P3 engine + metrics).
//
// # Persistence
//
//   - DB (Store, the Sink): research.hyperopt_studies (study.json + progress.json
//     folded into one row) + research.hyperopt_trials (trial artifacts), upserted
//     as trials complete, idempotent on (study_ts)/(study_ts, number).
//   - Legacy artifacts (artifacts.go): runs/hyperopt/<study_ts>/{study.json,
//     progress.json, trials/trial_%04d.json, best_params/<strat>.json}, byte
//     compatible with the Python reference schemas (shared pyjson encoder).
//   - Progress (gen/trial %) streamed to the jobs Redis channel via the handler's
//     reporting sink.
//
// # Promotion (Promoter, locked decision 6)
//
// Promote writes a chosen COMPLETE trial's params to tms.active_params with full
// audit (promoted_by/at, source_study, source_trial) via an immutable tuned
// tms.param_sets row (the §8.2 metadata-rewritten baseline). Joint studies
// promote all three sub-strategies. Idempotent and transactional.
//
// # Cancellation & resources
//
// ctx cancellation stops the study promptly (writing INTERRUPTED), with bounded
// memory (results pre-sized per generation, no unbounded buffering) and no leaked
// goroutines (the worker pool drains and joins on every generation boundary).
package study
