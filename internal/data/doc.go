// Package data owns market-data acquisition and storage: the Sharadar
// (Nasdaq Data Link) importers, the bar/universe repositories backed by
// TimescaleDB, and Parquet read/write (apache/arrow-go) for cache and
// interchange with the Python reference's cache/sharadar layout. It is the
// Go counterpart of src/data/, src/universe/ and src/cli/
// sync_sharadar_universe.py.
//
// Rules:
//   - All queries context-aware; bulk loads use COPY, not row-by-row.
//   - Imported datasets must be verifiable against the reference cache
//     (row counts, checksums) before being trusted by backtests.
package data
