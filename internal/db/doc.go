// Package db owns PostgreSQL/TimescaleDB connectivity: pgx v5 pool
// construction with sane defaults, and schema migrations via
// golang-migrate with migrations embedded from the top-level migrations
// package (iofs source, pgx5 database driver). Repositories in other packages
// receive a *pgxpool.Pool from here.
//
// Rules:
//   - Migrations are embedded in the binary; `tms migrate up` is the only
//     way schema changes reach an environment.
//   - All pool/migration operations are context-aware and produce wrapped,
//     actionable errors.
package db
