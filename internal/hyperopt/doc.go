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
package hyperopt
