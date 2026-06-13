-- Reverse 000008_jobs_p1.

DROP INDEX IF EXISTS tms.jobs_dedupe_active_idx;

ALTER TABLE tms.jobs
    DROP COLUMN IF EXISTS cancel_requested,
    DROP COLUMN IF EXISTS progress,
    DROP COLUMN IF EXISTS dedupe_key;
