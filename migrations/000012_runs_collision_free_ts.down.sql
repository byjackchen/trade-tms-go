-- Revert to the second-resolution-only run_ts CHECK. Any collision-free
-- run_ts rows written under 000012 would violate this; the down migration is
-- best-effort (the operator must purge sub-second run_ts rows first if any
-- exist). Restores the original implicit constraint from 000004.

ALTER TABLE tms.runs DROP CONSTRAINT IF EXISTS runs_run_ts_check;

ALTER TABLE tms.runs
    ADD CONSTRAINT runs_run_ts_check
    CHECK (run_ts ~ '^\d{4}-\d{2}-\d{2}_([01]\d|2[0-3])-[0-5]\d-[0-5]\d$');

COMMENT ON COLUMN tms.runs.run_ts IS NULL;
