// Package data owns market-data acquisition and storage: the Sharadar
// (Nasdaq Data Link) importers, the bar/universe repositories backed by
// TimescaleDB, and Parquet read/write (apache/arrow-go) for the cache/sharadar
// layout.
//
// Rules:
//   - All queries context-aware; bulk loads use COPY, not row-by-row.
//   - Imported datasets must be verifiable against the reference cache
//     (row counts, checksums) before being trusted by backtests.
package data
