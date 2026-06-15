-- 000013_scheduler_runs: single-leader, idempotent daily-pipeline ledger for
-- the NYSE-calendar-aware daily scheduler (internal/scheduler, `tms scheduler`).
--
-- Why a dedicated ledger rather than the tms.jobs dedupe_key index:
-- jobs_dedupe_active_idx (000008) only guarantees at most one ACTIVE
-- (queued|running) job per key — once a daily data.refresh succeeds, the key
-- is free again, so it cannot enforce "exactly ONE data.refresh per trading
-- day" across the whole day (a second scheduler tick after the first job
-- finished would happily re-enqueue). This table is the durable per-
-- (pipeline, trading_date) claim slot: the scheduler INSERTs the slot with
-- ON CONFLICT DO NOTHING and only enqueues the pipeline when the INSERT wins.
-- Multiple scheduler instances / a restart therefore enqueue the day's
-- pipeline AT MOST ONCE (single-leader without a separate lease) — the
-- single-replica compose service makes contention rare, but the unique key
-- makes it correct even if two run.

CREATE TABLE tms.scheduler_runs (
    id            BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    -- pipeline names the scheduled flow; today only 'daily' exists, but the
    -- column keeps the ledger open to future cadences (e.g. 'weekly') without
    -- a migration.
    pipeline      TEXT        NOT NULL CHECK (pipeline <> ''),
    -- trading_date is the America/New_York NYSE session date the run is FOR
    -- (the as_of of the EOD refresh / the catchup target), NOT the wall-clock
    -- date the scheduler fired — they can differ for a late catch-up.
    trading_date  DATE        NOT NULL,
    -- claimed_at stamps when this scheduler instance won the slot; claimed_by
    -- records which instance (host/pid) did, for operator forensics.
    claimed_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    claimed_by    TEXT        NOT NULL CHECK (claimed_by <> ''),
    -- The enqueued pipeline job ids (data.refresh, eod.refresh) for traceability.
    data_job_id   BIGINT,
    eod_job_id    BIGINT,
    -- trigger distinguishes the scheduled tick from a manual `tms sync now`
    -- / POST /api/v1/data/sync-now force.
    trigger       TEXT        NOT NULL DEFAULT 'scheduled'
                              CHECK (trigger IN ('scheduled', 'catchup', 'manual')),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE tms.scheduler_runs IS
    'Idempotent single-leader ledger for the daily incremental-sync scheduler: one row per (pipeline, trading_date) claim slot. The scheduler INSERTs ON CONFLICT DO NOTHING and only enqueues data.refresh + eod.refresh when the INSERT wins, so multiple instances / restarts never double-enqueue a trading day.';
COMMENT ON COLUMN tms.scheduler_runs.trading_date IS
    'America/New_York NYSE session date the run targets (EOD as_of / catchup target), not the wall-clock fire date.';
COMMENT ON COLUMN tms.scheduler_runs.trigger IS
    'scheduled = on-time daily tick; catchup = enqueued late on startup for a day whose configured time had already passed; manual = forced via `tms sync now` / POST /api/v1/data/sync-now.';

-- The dedupe guarantee: exactly one claim per (pipeline, trading_date).
CREATE UNIQUE INDEX scheduler_runs_slot_idx
    ON tms.scheduler_runs (pipeline, trading_date);
-- Operator history (newest first).
CREATE INDEX scheduler_runs_created_idx
    ON tms.scheduler_runs (created_at DESC);
