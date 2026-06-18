-- 000020_eod_ts_equals_asof (down): no-op. The original per-row source-bar `ts`
-- is NOT recoverable from the as_of date alone (ts_event_ns kept the source
-- instant, but the surviving collapsed row is the latest bar per symbol, not a
-- reversible mapping). This is a one-way convention realignment; rolling the
-- schema back does not need the old ts values, and re-running EOD would restamp
-- ts=as_of anyway. Intentionally empty.
SELECT 1;
