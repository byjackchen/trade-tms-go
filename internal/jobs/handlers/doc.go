// Package handlers contains the concrete job handlers dispatched by the
// internal/jobs worker.
//
// Currently implemented:
//
//   - data.refresh (DataRefresh): Sharadar data refresh. source=parquet
//     bulk-loads the Python reference's cache/sharadar parquet layout via
//     the P0 importer (internal/data/sharadar) — the backfill path.
//     source=api delegates to an injected APISyncer (the Nasdaq Data Link
//     incremental sync engine from the data-sync build phase); until that
//     is wired the api source fails fast with ErrAPISourceUnavailable.
//
// P1 locked decisions applied here:
//
//  1. Cache/data directories come exclusively from explicit configuration
//     (TMS_SHARADAR_CACHE_DIR); there is no repo-root discovery fallback
//     in the job path — an unset dir is a hard, immediate error naming the
//     variable. [IMPROVE over the Python repo-root walk]
//  2. All "today"/trading-date defaults inside sync implementations must
//     be the America/New_York trading date via internal/data/calendar
//     (documented on APISyncer; the Python reference mixes local and UTC
//     "today" — see docs/spec/data-sharadar.md Q3). [IMPROVE]
package handlers
