// Package nsga2 is a self-written, deterministic, seeded NSGA-II multi-objective
// optimizer for the hyperopt subsystem (docs/spec/hyperopt-metrics.md §6.4,
// locked decision 1).
//
// It reproduces the ALGORITHMIC CONFIGURATION of Optuna 4.8.0's NSGAIISampler —
// population_size 50, uniform crossover (crossover_prob 0.9, swapping_prob 0.5),
// per-gene drop-and-resample mutation (prob 1/max(1,n_params)), binary
// tournament selection on Pareto dominance, and fast-non-dominated-sort +
// crowding-distance elitist (mu+lambda) survival. It does NOT reproduce
// Optuna's NumPy MT19937 byte stream (Open Question Q1: semantic equivalence is
// the accepted contract). Correctness is therefore validated against the
// standard multi-objective test problems ZDT1/ZDT2 (known Pareto fronts) and a
// simple two-variable problem, plus strict same-seed determinism — NOT against
// Optuna's trial sequence.
//
// Determinism contract: a single seeded math/rand/v2 PCG generator threads the
// entire run (no global rand anywhere); the same seed plus the same evaluation
// results yields a byte-identical population trajectory. The population is
// rebuilt in stable id order every generation, so the result is independent of
// the order in which parallel evaluations complete (locked decision 5).
//
// The optimizer is pure with respect to its Evaluator: the parameter->objective
// mapping is injected (the orchestrator injects the backtest-based objective;
// tests inject synthetic objectives). Two interfaces are offered: the low-level
// Ask/Tell protocol (Optimizer) for the study coordinator that owns trial
// numbering / artifact writing / parallel dispatch, and the self-contained
// Optimize driver (with optional bounded-parallel fan-out and context
// cancellation) for tests and simple callers.
package nsga2
