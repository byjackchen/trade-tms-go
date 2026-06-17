-- 000003_strategy: strategy parameter management.
--
-- DB store for strategy params files
-- (docs/spec/hyperopt-metrics.md §2, §8):
-- param_sets stores immutable, versioned snapshots of a strategy's full
-- params document (the JSON file shape: strategy, schema_version, display,
-- allocation, metadata, parameters, constraints); active_params is the
-- promotion pointer (runs/active_params/<strategy>.json equivalent) with a
-- full audit trail.

-- ---------------------------------------------------------------------------
-- param_sets — versioned, immutable parameter documents.
-- ---------------------------------------------------------------------------
CREATE TABLE tms.param_sets (
    id               BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    strategy         TEXT        NOT NULL CHECK (strategy <> ''),
    version          INTEGER     NOT NULL CHECK (version >= 1),
    schema_version   INTEGER     NOT NULL DEFAULT 1 CHECK (schema_version >= 1),
    source           TEXT        NOT NULL CHECK (source IN ('baseline', 'tuned', 'manual', 'external')),
    payload          JSONB       NOT NULL CHECK (jsonb_typeof(payload) = 'object'),
    metadata         JSONB       NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(metadata) = 'object'),
    tuned_from_study TEXT,
    tuned_from_trial BIGINT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (strategy, version),
    -- tuned sets must carry their provenance (spec §8.2 metadata rewrite).
    CHECK (source <> 'tuned' OR tuned_from_study IS NOT NULL)
);

COMMENT ON TABLE tms.param_sets IS
    'Versioned strategy parameter documents. payload = full params JSON (strategy, schema_version, display, allocation, metadata, parameters in insertion order, constraints — spec hyperopt §2.1). Rows are immutable once referenced by active_params.';
COMMENT ON COLUMN tms.param_sets.source IS
    'baseline = shipped defaults; tuned = hyperopt best_params (metadata.source="tuned"); manual = operator edit; external = unrecognized provenance (spec §8.4 reader-side value).';
COMMENT ON COLUMN tms.param_sets.tuned_from_trial IS
    'Optuna trial number (NOT the artifact number — spec §6.5/§8.2); may have no trial_NNNN.json counterpart after resume drift (spec Q3).';

CREATE INDEX param_sets_strategy_idx ON tms.param_sets (strategy, version DESC);

CREATE TRIGGER param_sets_set_updated_at
    BEFORE UPDATE ON tms.param_sets
    FOR EACH ROW EXECUTE FUNCTION tms.set_updated_at();

-- ---------------------------------------------------------------------------
-- active_params — which param_set each strategy runs with (promotion target,
-- effect is next-run-only: live processes read params at startup, spec §8.4).
-- ---------------------------------------------------------------------------
CREATE TABLE tms.active_params (
    strategy     TEXT        NOT NULL PRIMARY KEY CHECK (strategy <> ''),
    param_set_id BIGINT      NOT NULL REFERENCES tms.param_sets (id) ON DELETE RESTRICT,
    source_id    TEXT        NOT NULL DEFAULT 'baseline'
                             CHECK (source_id = 'baseline'
                                    OR source_id = 'external'
                                    OR source_id ~ '^hyperopt:.+'),
    promoted_by  TEXT        NOT NULL CHECK (promoted_by <> ''),
    promoted_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    source_study TEXT,
    source_trial BIGINT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE tms.active_params IS
    'Promotion pointer per strategy (runs/active_params equivalent). No row = baseline (delete the row to revert — spec §8.4 set_active).';
COMMENT ON COLUMN tms.active_params.source_id IS
    'Grammar per spec §8.4: "baseline" | "hyperopt:<study_ts>" | "external" (reader-side only, never settable via API).';
COMMENT ON COLUMN tms.active_params.promoted_by IS 'Operator/user/automation identity that performed the promotion (audit).';
COMMENT ON COLUMN tms.active_params.source_trial IS 'Optuna trial number of the promoted best trial (audit; spec §8.1 step 2).';

CREATE INDEX active_params_param_set_idx ON tms.active_params (param_set_id);

CREATE TRIGGER active_params_set_updated_at
    BEFORE UPDATE ON tms.active_params
    FOR EACH ROW EXECUTE FUNCTION tms.set_updated_at();
