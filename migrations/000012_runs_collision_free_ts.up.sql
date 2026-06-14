-- 000012_runs_collision_free_ts: make tms.runs.run_ts collision-free under
-- concurrent backtests (TMS_WORKER_CONCURRENCY>1).
--
-- ROOT CAUSE: run_ts was constrained to SECOND resolution
-- (^\d{4}-\d{2}-\d{2}_HH-MM-SS$). Two backtests claimed within the same
-- wall-clock second generated the SAME run_ts; because run_ts is the
-- UNIQUE natural key and Store.Persist does an idempotent
-- DELETE-then-INSERT on it, one run's persisted rows (run, run_metrics,
-- equity_curves, trades) AND its runs/{ts} artifact dir were silently
-- destroyed by the other. The job still reported success with a run_id that
-- no longer existed — silent data loss.
--
-- FIX: relax the run_ts CHECK to ALSO permit the collision-free
-- %Y-%m-%d_%H-%M-%S-MMMMMM-CCCC form that runs.NewRunID emits
-- (a 6-digit microsecond field + a 4-digit per-process monotonic counter).
-- The auto-generated key path now uses NewRunID; the second-resolution form
-- stays valid so EXPLICIT caller-supplied idempotency keys (retried logical
-- runs) keep converging via DELETE-then-INSERT. The form remains lexically
-- sortable, so the run list's ORDER BY run_ts DESC still yields newest-first.
--
-- The constraint is the implicit name Postgres assigns the inline CHECK from
-- migration 000004 (table_column_check).

ALTER TABLE tms.runs DROP CONSTRAINT IF EXISTS runs_run_ts_check;

ALTER TABLE tms.runs
    ADD CONSTRAINT runs_run_ts_check
    CHECK (run_ts ~ '^\d{4}-\d{2}-\d{2}_([01]\d|2[0-3])-[0-5]\d-[0-5]\d(-\d{6}-\d{4})?$');

COMMENT ON COLUMN tms.runs.run_ts IS
    'UTC run key + runs/{ts} dir name. Auto-generated keys use the collision-free %Y-%m-%d_%H-%M-%S-MMMMMM-CCCC form (microsecond + per-process counter) so concurrent backtests never share the UNIQUE natural key; explicit idempotency keys may use the plain second-resolution form. Lexically sortable; list endpoints ORDER BY run_ts DESC (api spec §3.14).';
