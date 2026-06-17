-- 000015_models: first-class MODELS (named portfolio blueprints).
--
-- Before this, a "Model" did not exist as an entity: its three parts were
-- scattered + hardcoded (docs/concept-alignment.md §1.2) — members in
-- assembleMulti, weights in alloc* constants, composite risk in risk* constants
-- (with a sector special case). This collects all of it into data:
--   - tms.models:        the blueprint (cash reserve + composite risk + version).
--   - tms.model_members: which strategies, each weight, active flag, param ref.
--
-- All additions are purely additive (nothing dropped). The model_id columns on
-- sessions/runs/hyperopt_studies and accounts.default_model_id are NULLABLE: the
-- legacy "strategy=multi" / "strategy=sepa" callers are mapped to the seeded
-- Models by later phases, so existing rows stay valid until then.

-- ---------------------------------------------------------------------------
-- models — named, versionable portfolio blueprint.
-- ---------------------------------------------------------------------------
CREATE TABLE tms.models (
    -- id is a stable slug, e.g. 'default-multi', 'sepa-only', 'sepa-pairs-7030'.
    id                       TEXT             PRIMARY KEY CHECK (id <> ''),
    name                     TEXT             NOT NULL CHECK (name <> ''),
    description              TEXT             NOT NULL DEFAULT '',
    -- cash_pct is the uninvested reserve; Σ(active weights) + cash_pct <= 1.
    cash_pct                 DOUBLE PRECISION NOT NULL DEFAULT 0 CHECK (cash_pct >= 0 AND cash_pct < 1),
    -- composite (portfolio-level) risk fractions — Model is the single source.
    risk_single_name_pct     DOUBLE PRECISION NOT NULL CHECK (risk_single_name_pct > 0 AND risk_single_name_pct <= 1),
    risk_concentration_pct   DOUBLE PRECISION NOT NULL CHECK (risk_concentration_pct > 0 AND risk_concentration_pct <= 1),
    risk_daily_loss_halt_pct DOUBLE PRECISION NOT NULL CHECK (risk_daily_loss_halt_pct > 0 AND risk_daily_loss_halt_pct <= 1),
    risk_max_gross_pct       DOUBLE PRECISION CHECK (risk_max_gross_pct IS NULL OR risk_max_gross_pct > 0),
    risk_max_positions       INTEGER          CHECK (risk_max_positions IS NULL OR risk_max_positions > 0),
    version                  INTEGER          NOT NULL DEFAULT 1 CHECK (version >= 1),
    created_at               TIMESTAMPTZ      NOT NULL DEFAULT now(),
    updated_at               TIMESTAMPTZ      NOT NULL DEFAULT now()
);

COMMENT ON TABLE tms.models IS
    'Named portfolio blueprints: cash reserve + composite risk + version. Members (strategy + weight + param ref) live in tms.model_members. Drop-in target for backtest/optimize/paper/live (docs/concept-alignment.md §0).';

CREATE TRIGGER models_set_updated_at
    BEFORE UPDATE ON tms.models
    FOR EACH ROW EXECUTE FUNCTION tms.set_updated_at();

-- ---------------------------------------------------------------------------
-- model_members — which strategies a Model runs, each weight + param ref.
-- ---------------------------------------------------------------------------
CREATE TABLE tms.model_members (
    model_id     TEXT             NOT NULL REFERENCES tms.models (id) ON DELETE CASCADE,
    strategy_id  TEXT             NOT NULL CHECK (strategy_id IN ('sepa', 'sector_rotation', 'pairs', 'intraday_breakout')),
    weight       DOUBLE PRECISION NOT NULL CHECK (weight > 0 AND weight <= 1),  -- capital_pct
    active       BOOLEAN          NOT NULL DEFAULT true,
    -- param_set_id NULL = use that strategy's active params (tms.active_params).
    param_set_id BIGINT           REFERENCES tms.param_sets (id) ON DELETE RESTRICT,
    PRIMARY KEY (model_id, strategy_id)
);

COMMENT ON TABLE tms.model_members IS
    'A Model''s members: strategy + capital weight + active flag + optional param_set ref (NULL = strategy active params). Authoritative weight source (supersedes the params doc allocation block — docs/concept-alignment.md §3.1 C3).';

-- ---------------------------------------------------------------------------
-- Seed: backward-compatible Models for the legacy strategy= dispatch.
--   default-multi  ← old strategy=multi
--   {sepa,sector,pairs,orb}-only ← old strategy={sepa,sector_rotation,pairs,intraday_breakout}
-- param_set_id is NULL throughout (each member uses the strategy active params).
-- ---------------------------------------------------------------------------
INSERT INTO tms.models
    (id, name, description, cash_pct, risk_single_name_pct, risk_concentration_pct, risk_daily_loss_halt_pct) VALUES
    ('default-multi', 'Default Multi-Strategy', 'SEPA + Sector Rotation + Pairs blend (legacy strategy=multi).', 0.10, 0.50, 0.40, 0.10),
    ('sepa-only',     'SEPA Only',              'Single-member Model: SEPA (legacy strategy=sepa).',             0.00, 0.20, 0.30, 0.05),
    ('sector-only',   'Sector Rotation Only',   'Single-member Model: Sector Rotation (legacy strategy=sector_rotation).', 0.00, 0.50, 0.40, 0.10),
    ('pairs-only',    'Pairs Only',             'Single-member Model: Pairs (legacy strategy=pairs).',           0.00, 0.20, 0.30, 0.05),
    ('orb-only',      'Intraday ORB Only',      'Single-member Model: Intraday Breakout (legacy strategy=intraday_breakout).', 0.00, 0.20, 0.30, 0.05);

INSERT INTO tms.model_members (model_id, strategy_id, weight, active, param_set_id) VALUES
    ('default-multi', 'sepa',              0.40, true, NULL),
    ('default-multi', 'sector_rotation',   0.30, true, NULL),
    ('default-multi', 'pairs',             0.20, true, NULL),
    ('sepa-only',     'sepa',              1.00, true, NULL),
    ('sector-only',   'sector_rotation',   1.00, true, NULL),
    ('pairs-only',    'pairs',             1.00, true, NULL),
    ('orb-only',      'intraday_breakout', 1.00, true, NULL);

-- ---------------------------------------------------------------------------
-- model_id references on the runtime/history tables (all NULLABLE + additive).
-- ---------------------------------------------------------------------------
-- The Model the session actually ran (authoritative history).
ALTER TABLE tms.sessions         ADD COLUMN model_id TEXT REFERENCES tms.models (id) ON DELETE RESTRICT;
-- The Model the backtest ran (existing runs.strategies TEXT[] kept for display).
ALTER TABLE tms.runs             ADD COLUMN model_id TEXT REFERENCES tms.models (id) ON DELETE RESTRICT;
-- The Model an optimize/joint hyperopt study targets (existing strategy kept).
ALTER TABLE tms.hyperopt_studies ADD COLUMN model_id TEXT REFERENCES tms.models (id) ON DELETE RESTRICT;
-- The UI's default Model binding for an account.
ALTER TABLE tms.accounts         ADD COLUMN default_model_id TEXT REFERENCES tms.models (id) ON DELETE SET NULL;
