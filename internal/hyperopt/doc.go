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
// Top-level (this package), all parity-gated against the Python reference:
//   - walkforward.go: expanding_anchored split (spec §3, exact calendar-day
//     arithmetic incl. the vestigial-embargo quirk; 305-case parity fixture).
//   - search_spaces.go / loader.go / safe_eval.go: the per-strategy search-space
//     registry, the params loader (ordered parse + validation + defaults_dict +
//     suggest_with with in-order constraint clamping) and the AST-whitelisted
//     constraint expression evaluator (spec §2; baseline JSONs embedded;
//     suggest/safe_eval/validation parity fixtures).
//
// Subpackages (built and wired):
//   - nsga2/: the NSGA-II optimizer/coordinator (non-dominated sorting, crowding
//     distance, ask/tell loop). Depends only on this package + the standard
//     library; it carries no engine or persistence dependency.
//   - study/: the study coordinator — trial worker + per-fold stitch aggregation
//     wiring, study/progress/trial artifacts and best_params promotion. It is
//     the largest part of the subtree and is wired into the API
//     (api/handlers_hyperopt.go, api/stores.go) and the job runner
//     (jobs/handlers/hyperopt.go).
//
// Intra-subtree / cross-package dependencies of study:
//   - study → hyperopt, hyperopt/nsga2 (search + optimizer), engine +
//     engine/strategyassembly (trials run the real engine), params, portfolio,
//     metrics (Stitch / AggregateFolds, bit-exact after the Sharpe fix) and runs
//     (study/best_params persisted under the runs/hyperopt/ layout the UI source
//     selector expects).
package hyperopt
