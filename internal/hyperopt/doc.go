// Package hyperopt ports the Python reference's hyper-parameter
// optimization workflow (src/research/ + scripts/run_hyperopt): parameter
// space definition per strategy, search/trial scheduling over the backtest
// engine, scoring, and persistence of studies and best_params in the same
// layout the reference writes under runs/hyperopt/ so the UI source
// selector keeps working.
//
// Rules:
//   - Trials run the real engine — no shortcut evaluation paths.
//   - Studies are resumable; trial results are persisted as they complete.
//
// Implemented (P2 slice, all parity-gated against the Python reference):
//   - walkforward.go: expanding_anchored split (spec §3, exact calendar-day
//     arithmetic incl. the vestigial-embargo quirk; 305-case parity fixture).
//   - search_spaces.go / loader.go / safe_eval.go: the per-strategy search-space
//     registry, the params loader (ordered parse + validation + defaults_dict +
//     suggest_with with in-order constraint clamping) and the AST-whitelisted
//     constraint expression evaluator (spec §2; baseline JSONs embedded;
//     suggest/safe_eval/validation parity fixtures).
//
// Remaining (tracked, not yet built): the NSGA-II optimizer/coordinator, trial
// worker + per-fold stitch aggregation wiring, study/progress/trial artifacts,
// best_params promotion and the run_hyperopt CLI (spec §4-§11). The metric
// re-computation those depend on already lives in internal/metrics (Stitch /
// AggregateFolds), now bit-exact after the Sharpe fix.
package hyperopt
