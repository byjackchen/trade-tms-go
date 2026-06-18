-- 000020_eod_ts_equals_asof: realign existing EOD signal rows to the convention
-- "an EOD snapshot's `ts` is its as_of date" (publish/store.go rowArgs, upsert
-- branch). Before the fix, EOD stamped `ts` with the SOURCE BAR instant, so a
-- refresh started before its as_of bar was loaded stamped the prior bar's instant
-- — and two different as_of refreshes computed from the same latest bar collided
-- on `ts`, piling cross-as_of rows onto one ts. That broke the watchlist's
-- max(ts) frontier (it could anchor on a stale-but-higher-ts batch).
--
-- This migration retroactively sets every EOD row's `ts` to its as_of date
-- (midnight UTC, matching time.Date(Y,M,D,0,0,0,0,UTC)), so each as_of owns a
-- distinct ts. ts_event_ns is left untouched — it remains the exact source-bar
-- instant for audit/precision. Live rows (as_of IS NULL) keep their real-time ts.
-- Idempotent: only rows whose ts is not already as_of-midnight-UTC are touched.

UPDATE tms.signals
   SET ts = as_of::timestamp AT TIME ZONE 'UTC'
 WHERE as_of IS NOT NULL
   AND ts <> as_of::timestamp AT TIME ZONE 'UTC';
