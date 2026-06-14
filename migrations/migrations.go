// Package migrations embeds the golang-migrate SQL migration files that
// define the full TMS database schema (TimescaleDB / PostgreSQL).
//
// Files follow the golang-migrate naming convention
// NNNNNN_name.up.sql / NNNNNN_name.down.sql and are split by domain:
//
//	000001_init        — timescaledb extension + tms schema
//	000002_marketdata  — tickers, bars_daily, bars_intraday, fundamentals_sf1,
//	                     events, universe_snapshots, dataset_sync
//	000003_strategy    — param_sets, active_params
//	000004_research    — runs, run_metrics, equity_curves, trades,
//	                     hyperopt_studies, hyperopt_trials
//	000005_live        — sessions, orders, fills, positions, signal_intents,
//	                     risk_events, halts, reconciliation_reports
//	000006_ops         — jobs, commands, audit_log, app_config
//	000007_universe_members — universe_snapshots.members JSONB (ranked
//	                     members with score diagnostics + reasons)
//	000008_jobs_p1     — jobs P1 columns: dedupe_key (+ active-unique
//	                     index), progress JSONB, cancel_requested
//	000012_runs_collision_free_ts — relax runs.run_ts CHECK to permit the
//	                     collision-free %Y-%m-%d_%H-%M-%S-MMMMMM-CCCC form so
//	                     concurrent backtests never share the UNIQUE natural key
//
// Money convention (see docs/spec/domain-types-money.md): all USD price and
// balance columns are BIGINT fixed-point at 1e-4 scale
// (stored = dollars * 10000), matching the Go Money model. Quantities and
// volumes are plain BIGINT. Calendar dates are DATE; instants TIMESTAMPTZ.
//
// internal/db consumes Files through golang-migrate's iofs source; the
// `tms migrate up|down|status` subcommand is the only way schema changes
// reach an environment.
package migrations

import "embed"

// Files holds every embedded *.sql migration, addressed from the package
// root (use iofs.New(Files, ".")).
//
//go:embed *.sql
var Files embed.FS
