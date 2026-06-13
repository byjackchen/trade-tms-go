-- 000007_dataset_sync_runs: per-run sync audit trail for the Sharadar
-- API -> PG incremental sync (internal/data/sharadar Syncer).
--
-- tms.dataset_sync (000002) stays the watermark table (CacheMeta parity:
-- one row per dataset, last wall-clock sync + cumulative row count).
-- This table is additive [IMPROVE]: one row per dataset per sync/bootstrap
-- run with started/finished timestamps, net-new row count, status and
-- error text, so operators can audit catchup history and partial failures
-- (the Python reference only keeps the latest .meta.json snapshot).

CREATE TABLE tms.dataset_sync_runs (
    id          BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    dataset     TEXT        NOT NULL
                            CHECK (dataset IN ('TICKERS', 'SEP', 'SFP', 'SF1', 'EVENTS')),
    kind        TEXT        NOT NULL CHECK (kind IN ('bootstrap', 'catchup', 'import')),
    started_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ,
    rows_added  BIGINT      NOT NULL DEFAULT 0 CHECK (rows_added >= 0),
    status      TEXT        NOT NULL DEFAULT 'running'
                            CHECK (status IN ('running', 'ok', 'error')),
    error       TEXT,
    CHECK (finished_at IS NULL OR finished_at >= started_at),
    CHECK (status <> 'running' OR finished_at IS NULL),
    CHECK (status <> 'error' OR error IS NOT NULL)
);

COMMENT ON TABLE tms.dataset_sync_runs IS
    'Append-only audit of Sharadar sync runs: one row per dataset per run. rows_added = net-new keys (Python writers'' added semantics, spec data-sharadar.md §6); status=error keeps the warn-and-continue failure text (spec §8).';
COMMENT ON COLUMN tms.dataset_sync_runs.kind IS
    'bootstrap = full backfill (spec §9), catchup = ensure_cache_fresh-style incremental (spec §8), import = parquet cache bulk load.';

CREATE INDEX dataset_sync_runs_dataset_started_idx
    ON tms.dataset_sync_runs (dataset, started_at DESC);
