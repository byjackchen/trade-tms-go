// Package sharadar implements the Sharadar parquet-cache importer: it reads
// the Python reference repo's cache/sharadar layout (docs/spec/data-sharadar.md
// §4) with apache/arrow-go's parquet reader, converts rows to the domain's
// numeric model (float64 prices -> int64 1e-4 fixed point via the
// Decimal(str(x)) half-even bridge, NaN -> NULL), and bulk-loads them into
// TimescaleDB via pgx CopyFrom into a session temp staging table followed by
// INSERT ... ON CONFLICT merges.
//
// Why staging + ON CONFLICT instead of delete-range + COPY:
//
//   - Idempotency without a destructive window: re-running the importer (or
//     running it with --tickers/--since filters) never deletes rows outside
//     the imported slice, and concurrent API readers never observe a gap.
//   - Merge parity: the Python writers merge with drop_duplicates(keep="last")
//     — "new rows win" on the dedup key (spec §6). ON CONFLICT DO UPDATE is
//     the exact relational equivalent; a DISTINCT ON (key ... ORDER BY seq
//     DESC) pre-pass inside the merge keeps last-wins semantics even if a
//     single staged batch carried key duplicates.
//   - Bulk speed is retained: CopyFrom targets the temp table (session-local,
//     not WAL-logged), and the merge is one set-based INSERT per batch.
//   - created_at survives revisions and the updated_at trigger records them,
//     which delete+copy would silently reset.
//
// TICKERS is the one exception in spirit: the Python writer fully overwrites
// TICKERS.parquet on every sync (spec §2.5), so when no --tickers filter is
// active the importer additionally deletes tms.tickers rows absent from the
// source file, inside the same transaction as the upsert.
//
// Error policy: per-file failures are captured into the run summary and the
// run continues (mirroring catchup's warn-and-continue, spec §8); per-row
// conversion failures (missing key fields, invalid dimension, ...) skip the
// row and are counted; per-field unrepresentable values (±Inf, |price| above
// the int64 1e-4 range — physically impossible for real Sharadar data, whose
// consumer-side cap is 1.7e13 USD) are stored as NULL and counted. Context
// cancellation aborts the run promptly and is never swallowed.
package sharadar
