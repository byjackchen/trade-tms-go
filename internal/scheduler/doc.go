// Package scheduler is the NYSE-calendar-aware daily incremental-sync
// scheduler: on every trading day, a configured time after the US close, it
// ENQUEUES the daily data pipeline onto the durable tms.jobs queue
// (internal/jobs) so the market data store keeps each session's new data
// fresh automatically without an operator.
//
// # Pipeline
//
// For a trading date D the scheduler enqueues, in order:
//
//  1. data.refresh source=api — the watermark-driven Nasdaq Data Link
//     incremental catchup (sep/sfp/sf1/events through T-1), via the existing
//     handlers.KindDataRefresh handler / internal/data/sharadar.Syncer.
//  2. eod.refresh as_of=D — the idempotent signal-intent precompute, gated to
//     run AFTER the data refresh succeeds (the eod job is enqueued with a
//     run_at floor; correctness still holds if it runs early because eod is
//     idempotent, but the floor avoids re-replaying stale bars).
//
// # Single-leader idempotency
//
// Enqueue happens AT MOST ONCE per (pipeline, trading_date) even if multiple
// scheduler instances run or the process restarts mid-day. The guarantee is a
// durable claim slot in tms.scheduler_runs (migration 000013): the scheduler
// INSERTs the (pipeline, trading_date) row ON CONFLICT DO NOTHING and only
// enqueues the jobs when the INSERT wins. The active-only jobs.dedupe_key
// index cannot provide this — a succeeded daily job frees its key, so a later
// tick would re-enqueue.
//
// # Trading-date discipline
//
// Every "today"/"trading date" is the America/New_York NYSE session date
// resolved through internal/data/calendar (never bare UTC/local date math),
// matching the rest of the system (P1 locked decision 2). Weekends, holidays
// and special closures are skipped.
//
// # Catch-up
//
// On startup, if today is a trading day whose configured fire time has
// already passed and the day's pipeline was never enqueued, the scheduler
// enqueues it once (trigger=catchup) so a restart does not skip the day.
// Disable with TMS_SCHEDULER_CATCHUP=false.
//
// # Clock
//
// The scheduler takes an injectable now() func so tests drive it with a
// controllable clock (assert exactly-once-per-day, weekend/holiday skips,
// dedupe across restarts, catch-up) without sleeping or touching the wall
// clock; production passes time.Now.
package scheduler
