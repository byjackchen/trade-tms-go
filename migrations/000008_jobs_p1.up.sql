-- 000008_jobs_p1: P1 job-orchestration additions to tms.jobs.
--
--   dedupe_key       — optional logical identity; at most ONE active
--                      (queued|running) job may carry a given key. Enforced
--                      by the partial unique index jobs_dedupe_active_idx,
--                      consumed by Enqueue's
--                      ON CONFLICT (dedupe_key) WHERE status IN ('queued','running').
--                      Terminal jobs free the key, so reruns are allowed.
--   progress         — handler-reported progress object (live UI; also
--                      mirrored to Redis pub/sub channel tms:jobs:events).
--   cancel_requested — cooperative-cancel flag. Cancel of a *queued* job
--                      flips status to 'canceled' directly; cancel of a
--                      *running* job sets this flag, which the owning
--                      worker observes via its heartbeat round-trip and
--                      then cancels the handler context. The worker (or
--                      the stale-claim reaper, if the worker died) writes
--                      the terminal 'canceled' state.
--
-- Status spelling note: the queue uses 'canceled' (single l), fixed by the
-- 000006 CHECK constraint; internal/jobs mirrors that spelling.

ALTER TABLE tms.jobs
    ADD COLUMN dedupe_key       TEXT    CHECK (dedupe_key <> ''),
    ADD COLUMN progress         JSONB   CHECK (progress IS NULL OR jsonb_typeof(progress) = 'object'),
    ADD COLUMN cancel_requested BOOLEAN NOT NULL DEFAULT false;

COMMENT ON COLUMN tms.jobs.dedupe_key IS
    'Optional logical identity; unique among active (queued|running) jobs via jobs_dedupe_active_idx. NULL = no dedupe.';
COMMENT ON COLUMN tms.jobs.progress IS
    'Latest handler-reported progress object (JSONB); also published to Redis channel tms:jobs:events for live UI.';
COMMENT ON COLUMN tms.jobs.cancel_requested IS
    'Cooperative cancel flag for running jobs; the owning worker observes it on heartbeat and cancels the handler context.';

-- Arbiter index for Enqueue dedupe. Predicate must stay exactly
-- "status IN ('queued', 'running')" — internal/jobs infers it in
-- ON CONFLICT, and PostgreSQL only matches partial unique indexes whose
-- predicate is implied by the inference clause.
CREATE UNIQUE INDEX jobs_dedupe_active_idx
    ON tms.jobs (dedupe_key)
    WHERE status IN ('queued', 'running');
