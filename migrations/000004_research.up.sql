-- 000004_research: backtest runs + hyperopt studies.
--
-- DB counterpart of the Python reference's runs/{ts}/ dumps and
-- runs/hyperopt/{study_ts}/ artifact trees (docs/spec/hyperopt-metrics.md
-- §6-§9, api-ws-redis.md §3.11-§3.22). Money columns are BIGINT fixed-point
-- at 1e-4 USD scale (stored = dollars * 10000); sharpe/calmar/max_drawdown
-- stay DOUBLE PRECISION because the reference computes them in IEEE-754
-- float64 and parity is asserted at float64 precision (hyperopt spec §1).

-- ---------------------------------------------------------------------------
-- runs — one row per backtest run (meta.json equivalent).
-- ---------------------------------------------------------------------------
CREATE TABLE tms.runs (
    id                   BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    run_ts               TEXT        NOT NULL UNIQUE
                                     CHECK (run_ts ~ '^\d{4}-\d{2}-\d{2}_([01]\d|2[0-3])-[0-5]\d-[0-5]\d$'),
    kind                 TEXT        NOT NULL DEFAULT 'multi-strategy' CHECK (kind <> ''),
    status               TEXT        NOT NULL DEFAULT 'RUNNING'
                                     CHECK (status IN ('RUNNING', 'COMPLETE', 'INTERRUPTED', 'FAIL')),
    start_date           DATE        NOT NULL,
    end_date             DATE        NOT NULL CHECK (end_date >= start_date),
    starting_balance_usd BIGINT      NOT NULL CHECK (starting_balance_usd > 0),
    final_balance_usd    BIGINT,
    total_pnl_usd        BIGINT,
    strategies           TEXT[]      NOT NULL DEFAULT '{}',
    config               JSONB       NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(config) = 'object'),
    meta                 JSONB       NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(meta) = 'object'),
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- a finished run must report its final balance and P&L
    CHECK (status <> 'COMPLETE' OR (final_balance_usd IS NOT NULL AND total_pnl_usd IS NOT NULL))
);

COMMENT ON TABLE tms.runs IS
    'Backtest runs (runs/{ts}/meta.json equivalent). run_ts = UTC %Y-%m-%d_%H-%M-%S directory name; list endpoints sort by it descending (api spec §3.14).';
COMMENT ON COLUMN tms.runs.starting_balance_usd IS 'USD fixed-point 1e-4 (default run: 100000 USD = 1000000000).';
COMMENT ON COLUMN tms.runs.config IS 'StrategyConfigOverrides + run flags as supplied (hyperopt spec §1.6).';

CREATE INDEX runs_created_idx ON tms.runs (run_ts DESC);
CREATE INDEX runs_status_idx ON tms.runs (status) WHERE status = 'RUNNING';

CREATE TRIGGER runs_set_updated_at
    BEFORE UPDATE ON tms.runs
    FOR EACH ROW EXECUTE FUNCTION tms.set_updated_at();

-- ---------------------------------------------------------------------------
-- run_metrics — BacktestMetrics per run, per scope ('portfolio' or a
-- strategy_id). Exact field set of hyperopt spec §1.1.
-- ---------------------------------------------------------------------------
CREATE TABLE tms.run_metrics (
    run_id              BIGINT           NOT NULL REFERENCES tms.runs (id) ON DELETE CASCADE,
    scope               TEXT             NOT NULL DEFAULT 'portfolio' CHECK (scope <> ''),
    final_balance_usd   BIGINT           NOT NULL,
    total_pnl_usd       BIGINT           NOT NULL,
    sharpe              DOUBLE PRECISION NOT NULL,
    calmar              DOUBLE PRECISION NOT NULL,
    max_drawdown_pct    DOUBLE PRECISION NOT NULL CHECK (max_drawdown_pct <= 0),
    num_orders          INTEGER          NOT NULL CHECK (num_orders >= 0),
    num_filled_orders   INTEGER          NOT NULL CHECK (num_filled_orders >= 0),
    num_rejected_orders INTEGER          NOT NULL CHECK (num_rejected_orders >= 0),
    num_positions       INTEGER          NOT NULL CHECK (num_positions >= 0),
    extra               JSONB            NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(extra) = 'object'),
    created_at          TIMESTAMPTZ      NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ      NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, scope)
);

COMMENT ON TABLE tms.run_metrics IS
    'BacktestMetrics (hyperopt spec §1.1): money in BIGINT 1e-4 USD; sharpe/calmar/max_drawdown_pct in float64 exactly as computed (population std-dev, 252 periods/yr, calmar zero-DD floor 0.01). max_drawdown_pct is non-positive percent units.';

CREATE TRIGGER run_metrics_set_updated_at
    BEFORE UPDATE ON tms.run_metrics
    FOR EACH ROW EXECUTE FUNCTION tms.set_updated_at();

-- ---------------------------------------------------------------------------
-- equity_curves — EOD mark-to-market samples per run/scope (hypertable).
-- ---------------------------------------------------------------------------
CREATE TABLE tms.equity_curves (
    run_id      BIGINT      NOT NULL REFERENCES tms.runs (id) ON DELETE CASCADE,
    scope       TEXT        NOT NULL DEFAULT 'portfolio' CHECK (scope <> ''),
    ts          TIMESTAMPTZ NOT NULL,
    balance_usd BIGINT      NOT NULL,
    PRIMARY KEY (run_id, scope, ts)
);

COMMENT ON TABLE tms.equity_curves IS
    'Equity curve points (account.json / strategy_equity equivalents). Per-strategy samples are summed per timestamp into the portfolio curve, chronological order (hyperopt spec §1.6). balance_usd = USD fixed-point 1e-4.';

-- Hypertable, uncompressed by design: rows are small, and ON DELETE CASCADE
-- from tms.runs must stay cheap (compressed chunks make FK cascades costly).
SELECT create_hypertable('tms.equity_curves', 'ts', chunk_time_interval => INTERVAL '1 year');

-- ---------------------------------------------------------------------------
-- trades — round-trip trades extracted from run dumps (positions.json).
-- ---------------------------------------------------------------------------
CREATE TABLE tms.trades (
    id               BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    run_id           BIGINT      NOT NULL REFERENCES tms.runs (id) ON DELETE CASCADE,
    strategy_id      TEXT        NOT NULL CHECK (strategy_id <> ''),
    symbol           TEXT        NOT NULL CHECK (symbol <> ''),
    side             TEXT        NOT NULL CHECK (side IN ('LONG', 'SHORT')),
    qty              BIGINT      NOT NULL CHECK (qty > 0),
    entry_ts         TIMESTAMPTZ NOT NULL,
    exit_ts          TIMESTAMPTZ CHECK (exit_ts IS NULL OR exit_ts >= entry_ts),
    entry_px         BIGINT      NOT NULL CHECK (entry_px > 0),
    exit_px          BIGINT      CHECK (exit_px IS NULL OR exit_px > 0),
    realized_pnl_usd BIGINT,
    fees_usd         BIGINT      NOT NULL DEFAULT 0,
    meta             JSONB       NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(meta) = 'object'),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- an exited trade must carry its exit price
    CHECK (exit_ts IS NULL OR exit_px IS NOT NULL)
);

COMMENT ON TABLE tms.trades IS
    'Research trades per run. qty unsigned, side encodes direction (portfolio spec §1.2 convention). Prices/fees/P&L = USD fixed-point 1e-4. meta carries the raw engine blob.';

CREATE INDEX trades_run_strategy_idx ON tms.trades (run_id, strategy_id);
CREATE INDEX trades_run_symbol_idx ON tms.trades (run_id, symbol);

-- ---------------------------------------------------------------------------
-- hyperopt_studies — study.json + progress.json folded into one row.
-- ---------------------------------------------------------------------------
CREATE TABLE tms.hyperopt_studies (
    study_ts          TEXT        NOT NULL PRIMARY KEY
                                  CHECK (study_ts ~ '^\d{4}-\d{2}-\d{2}_([01]\d|2[0-3])-[0-5]\d-[0-5]\d$'),
    study_name        TEXT        NOT NULL UNIQUE CHECK (study_name <> ''),
    strategy          TEXT        NOT NULL CHECK (strategy IN ('sepa', 'sector_rotation', 'pairs', 'joint')),
    start_date        DATE        NOT NULL,
    end_date          DATE        NOT NULL CHECK (end_date >= start_date),
    directions        TEXT[]      NOT NULL DEFAULT ARRAY['maximize', 'maximize'],
    objectives        TEXT[]      NOT NULL DEFAULT ARRAY['sharpe', 'calmar'],
    seed              BIGINT      NOT NULL DEFAULT 42,
    n_trials          INTEGER     NOT NULL CHECK (n_trials >= 1),
    workers           INTEGER     NOT NULL DEFAULT 1 CHECK (workers >= 1),
    walk_forward      BOOLEAN     NOT NULL DEFAULT TRUE,
    folds             INTEGER     NOT NULL DEFAULT 5 CHECK (folds >= 1),
    embargo_days      INTEGER     NOT NULL DEFAULT 5 CHECK (embargo_days >= 0),
    dump_trials       BOOLEAN     NOT NULL DEFAULT TRUE,
    trial_timeout_sec INTEGER     CHECK (trial_timeout_sec IS NULL OR trial_timeout_sec > 0),
    status            TEXT        NOT NULL DEFAULT 'RUNNING'
                                  CHECK (status IN ('RUNNING', 'INTERRUPTED', 'COMPLETE')),
    completed_trials  INTEGER     NOT NULL DEFAULT 0 CHECK (completed_trials >= 0),
    failed_trials     INTEGER     NOT NULL DEFAULT 0 CHECK (failed_trials >= 0),
    running_trials    INTEGER     NOT NULL DEFAULT 0 CHECK (running_trials >= 0),
    started_at        TIMESTAMPTZ,
    last_heartbeat_at TIMESTAMPTZ,
    coordinator_pid   INTEGER,
    current_best      JSONB       CHECK (current_best IS NULL OR jsonb_typeof(current_best) = 'object'),
    last_error        TEXT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE tms.hyperopt_studies IS
    'Hyperopt studies: study.json identity/config (hyperopt spec §7.2) + progress.json live state (§7.3). status vocabulary RUNNING|INTERRUPTED|COMPLETE written by the coordinator; UNKNOWN is synthesized by API readers only, never stored. Heartbeat every 20s; staleness threshold 60s (§6.10/§9.2).';
COMMENT ON COLUMN tms.hyperopt_studies.trial_timeout_sec IS 'NULL disables the per-trial timeout (CLI 0 maps to NULL — hyperopt spec §11).';
COMMENT ON COLUMN tms.hyperopt_studies.current_best IS
    '{"trial": <artifact number>, "sharpe": float, "calmar": float} — argmax of sharpe+calmar over COMPLETE trials, first-seen wins ties (hyperopt spec §6.8).';

CREATE INDEX hyperopt_studies_strategy_idx ON tms.hyperopt_studies (strategy, study_ts DESC);

CREATE TRIGGER hyperopt_studies_set_updated_at
    BEFORE UPDATE ON tms.hyperopt_studies
    FOR EACH ROW EXECUTE FUNCTION tms.set_updated_at();

-- ---------------------------------------------------------------------------
-- hyperopt_trials — trials/trial_%04d.json equivalent.
-- ---------------------------------------------------------------------------
CREATE TABLE tms.hyperopt_trials (
    study_ts      TEXT             NOT NULL REFERENCES tms.hyperopt_studies (study_ts) ON DELETE CASCADE,
    number        INTEGER          NOT NULL CHECK (number >= 0),
    optuna_number INTEGER          CHECK (optuna_number IS NULL OR optuna_number >= 0),
    strategy      TEXT             NOT NULL CHECK (strategy <> ''),
    params        JSONB            NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(params) = 'object'),
    metrics       JSONB            NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(metrics) = 'object'),
    folds         JSONB            NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(folds) = 'array'),
    state         TEXT             NOT NULL CHECK (state IN ('RUNNING', 'COMPLETE', 'FAIL')),
    sharpe        DOUBLE PRECISION,
    calmar        DOUBLE PRECISION,
    started_at    TIMESTAMPTZ      NOT NULL,
    finished_at   TIMESTAMPTZ      CHECK (finished_at IS NULL OR finished_at >= started_at),
    duration_sec  DOUBLE PRECISION NOT NULL DEFAULT 0 CHECK (duration_sec >= 0),
    run_dump_ts   TEXT,
    error         TEXT,
    created_at    TIMESTAMPTZ      NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ      NOT NULL DEFAULT now(),
    PRIMARY KEY (study_ts, number),
    -- artifact contract (hyperopt spec §5.4/§7.4): FAIL ⇒ empty metrics/folds
    -- + an error message; COMPLETE ⇒ objective values present.
    CHECK (state <> 'FAIL' OR error IS NOT NULL),
    CHECK (state <> 'COMPLETE' OR (sharpe IS NOT NULL AND calmar IS NOT NULL))
);

COMMENT ON TABLE tms.hyperopt_trials IS
    'Hyperopt trial artifacts (trial_%04d.json, hyperopt spec §7.4). number = artifact number 0..n_trials-1 (file identity, re-run/overwritten when not COMPLETE on resume — §6.5); optuna_number = sampler-side trial number, drifts across resumes (Q3). params = pre-constraint-clamp suggested values (§2.3/Q5); folds = per-fold metric payloads in fold order, [] when single-window or FAIL.';
COMMENT ON COLUMN tms.hyperopt_trials.sharpe IS 'Denormalized from metrics for current_best / Pareto queries; float64 parity with the reference.';
COMMENT ON COLUMN tms.hyperopt_trials.error IS 'FAIL detail; timeouts use the "timeout: trial timeout after <N>s" shape (hyperopt spec §5.4).';

CREATE INDEX hyperopt_trials_state_idx ON tms.hyperopt_trials (study_ts, state);
CREATE INDEX hyperopt_trials_complete_objectives_idx
    ON tms.hyperopt_trials (study_ts, sharpe DESC, calmar DESC)
    WHERE state = 'COMPLETE';
