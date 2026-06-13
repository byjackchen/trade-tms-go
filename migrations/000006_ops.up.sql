-- 000006_ops: operations domain — job queue, control commands, audit log,
-- runtime configuration.

-- ---------------------------------------------------------------------------
-- jobs — durable work queue, designed for FOR UPDATE SKIP LOCKED claiming.
--
-- Claim pattern (one statement, race-free under concurrency):
--
--   UPDATE tms.jobs j
--      SET status = 'running', claimed_by = $1, claimed_at = now(),
--          started_at = now(), heartbeat_at = now(), attempts = attempts + 1
--    WHERE j.id = (
--          SELECT id FROM tms.jobs
--           WHERE status = 'queued' AND run_at <= now()
--           ORDER BY priority DESC, run_at, id
--           FOR UPDATE SKIP LOCKED
--           LIMIT 1)
--   RETURNING j.*;
--
-- Workers heartbeat by bumping heartbeat_at; a reaper re-queues running jobs
-- whose heartbeat is stale and attempts < max_attempts, else marks failed.
-- ---------------------------------------------------------------------------
CREATE TABLE tms.jobs (
    id           BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    kind         TEXT        NOT NULL CHECK (kind <> ''),
    payload      JSONB       NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(payload) = 'object'),
    status       TEXT        NOT NULL DEFAULT 'queued'
                             CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'canceled')),
    priority     INTEGER     NOT NULL DEFAULT 0,
    run_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    attempts     INTEGER     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    max_attempts INTEGER     NOT NULL DEFAULT 1 CHECK (max_attempts >= 1),
    claimed_by   TEXT,
    claimed_at   TIMESTAMPTZ,
    heartbeat_at TIMESTAMPTZ,
    started_at   TIMESTAMPTZ,
    finished_at  TIMESTAMPTZ CHECK (finished_at IS NULL OR started_at IS NULL OR finished_at >= started_at),
    last_error   TEXT,
    result       JSONB       CHECK (result IS NULL OR jsonb_typeof(result) = 'object'),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- state-machine consistency
    CHECK (status <> 'running' OR (claimed_by IS NOT NULL AND claimed_at IS NOT NULL)),
    CHECK (status NOT IN ('succeeded', 'failed', 'canceled') OR finished_at IS NOT NULL),
    CHECK (status <> 'failed' OR last_error IS NOT NULL)
);

COMMENT ON TABLE tms.jobs IS
    'Durable job queue (Sharadar catchup days, EOD refresh, imports, hyperopt trials, ...). Claiming uses FOR UPDATE SKIP LOCKED over the jobs_claim_idx partial index; see table DDL header for the canonical statement.';
COMMENT ON COLUMN tms.jobs.kind IS 'Worker dispatch key, dotted lowercase (e.g. sharadar.catchup, eod.refresh, import.sep).';
COMMENT ON COLUMN tms.jobs.run_at IS 'Earliest eligible execution time (scheduling / retry backoff).';
COMMENT ON COLUMN tms.jobs.heartbeat_at IS 'Liveness stamp from the claiming worker; stale heartbeat => reaper re-queues or fails the job.';

-- Exactly the claim ORDER BY, restricted to claimable rows.
CREATE INDEX jobs_claim_idx ON tms.jobs (priority DESC, run_at, id) WHERE status = 'queued';
-- Reaper scan for stuck running jobs.
CREATE INDEX jobs_running_heartbeat_idx ON tms.jobs (heartbeat_at) WHERE status = 'running';
CREATE INDEX jobs_kind_status_idx ON tms.jobs (kind, status, created_at DESC);

CREATE TRIGGER jobs_set_updated_at
    BEFORE UPDATE ON tms.jobs
    FOR EACH ROW EXECUTE FUNCTION tms.set_updated_at();

-- ---------------------------------------------------------------------------
-- commands — operator/UI control-plane requests to running components.
-- ---------------------------------------------------------------------------
CREATE TABLE tms.commands (
    id              BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    source          TEXT        NOT NULL CHECK (source IN ('api', 'cli', 'ui', 'system')),
    target          TEXT        NOT NULL CHECK (target <> ''),
    name            TEXT        NOT NULL CHECK (name <> ''),
    args            JSONB       NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(args) = 'object'),
    status          TEXT        NOT NULL DEFAULT 'pending'
                                CHECK (status IN ('pending', 'acknowledged', 'completed', 'rejected', 'expired')),
    requested_by    TEXT        NOT NULL CHECK (requested_by <> ''),
    acknowledged_at TIMESTAMPTZ,
    completed_at    TIMESTAMPTZ CHECK (completed_at IS NULL OR acknowledged_at IS NULL OR completed_at >= acknowledged_at),
    result          JSONB       CHECK (result IS NULL OR jsonb_typeof(result) = 'object'),
    error           TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (status <> 'rejected' OR error IS NOT NULL)
);

COMMENT ON TABLE tms.commands IS
    'Control-plane commands (halt/resume, source promotion, sync triggers). target = component id (live, eod, api). The trading mutation surface stays out of the HTTP API by design (api spec §1.1 — read-only forever); commands are the audited side channel.';

CREATE INDEX commands_pending_idx ON tms.commands (target, created_at) WHERE status = 'pending';
CREATE INDEX commands_target_idx ON tms.commands (target, created_at DESC);

CREATE TRIGGER commands_set_updated_at
    BEFORE UPDATE ON tms.commands
    FOR EACH ROW EXECUTE FUNCTION tms.set_updated_at();

-- ---------------------------------------------------------------------------
-- audit_log — append-only record of every state-changing action.
-- ---------------------------------------------------------------------------
CREATE TABLE tms.audit_log (
    id        BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    ts        TIMESTAMPTZ NOT NULL DEFAULT now(),
    actor     TEXT        NOT NULL CHECK (actor <> ''),
    action    TEXT        NOT NULL CHECK (action <> ''),
    entity    TEXT,
    entity_id TEXT,
    details   JSONB       NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(details) = 'object'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE tms.audit_log IS
    'Append-only audit trail (param promotions, halts, manual interventions, schema migrations of data). Rows are never updated or deleted by application code.';

CREATE INDEX audit_log_ts_idx ON tms.audit_log (ts DESC);
CREATE INDEX audit_log_entity_idx ON tms.audit_log (entity, entity_id, ts DESC);

-- ---------------------------------------------------------------------------
-- app_config — runtime key/value configuration.
-- ---------------------------------------------------------------------------
CREATE TABLE tms.app_config (
    key         TEXT        NOT NULL PRIMARY KEY CHECK (key <> ''),
    value       JSONB       NOT NULL,
    description TEXT,
    updated_by  TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE tms.app_config IS
    'Runtime configuration overrides, dotted lowercase keys (e.g. live.universe_limit, risk.daily_loss_halt_pct). Env vars remain the bootstrap source; rows here are operator-visible, audited overrides.';

CREATE TRIGGER app_config_set_updated_at
    BEFORE UPDATE ON tms.app_config
    FOR EACH ROW EXECUTE FUNCTION tms.set_updated_at();
