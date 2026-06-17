-- 000002_marketdata: market-data domain.
--
-- Loads the Sharadar parquet cache (docs/spec/data-sharadar.md) into
-- TimescaleDB. Conventions:
--
--   * MONEY: USD price columns are BIGINT fixed-point at 1e-4 scale
--     (stored = dollars * 10000), matching the Go Money model. The Sharadar
--     price cap used by consumers (17_014_118_346_046.0) scales to 1.7e17,
--     comfortably inside int64. Storing fixed-point instead of the cache's
--     raw float64 is sanctioned by the [IMPROVE] note in
--     docs/spec/data-sharadar.md §2.1 (with §12 scoping the float64
--     round-trip gate to the parquet layer): unrepresentable values
--     (±Inf, >9.22e14 USD — empirically 3,479 cells of one ticker, BINI)
--     are stored NULL and counted via ImportStats.FieldsNulled.
--   * Source NaN doubles (prices, volume) map to NULL — NaN-containing
--     tickers are dropped by CONSUMERS, never cleaned in the store
--     (spec §2.1), so the store must be able to represent them.
--   * Trading dates are stored as TIMESTAMPTZ at UTC midnight on hypertables
--     (Timescale partitions on a time column) and as DATE elsewhere; the
--     cache layer is tz-naive-midnight, the engine re-attaches UTC
--     (spec §2.6) — UTC midnight is the same instant.

-- Shared helper: keep updated_at honest on every UPDATE.
CREATE OR REPLACE FUNCTION tms.set_updated_at() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    NEW.updated_at := now();
    RETURN NEW;
END;
$$;

COMMENT ON FUNCTION tms.set_updated_at() IS
    'Row trigger: stamps updated_at = now() on UPDATE. Attach to every table that carries updated_at.';

-- ---------------------------------------------------------------------------
-- tickers — universe master (SHARADAR/TICKERS, filtered + column-pruned at
-- write time per spec §2.5: SF1 domestic common stock incl. delisted, SFP
-- active funds only; full overwrite per sync).
-- ---------------------------------------------------------------------------
CREATE TABLE tms.tickers (
    ticker         TEXT        NOT NULL PRIMARY KEY CHECK (ticker <> ''),
    name           TEXT,
    exchange       TEXT,
    is_delisted    BOOLEAN     NOT NULL DEFAULT FALSE,
    category       TEXT,
    sector         TEXT,
    industry       TEXT,
    table_name     TEXT        NOT NULL CHECK (table_name IN ('SF1', 'SFP')),
    first_price_date DATE,
    last_price_date  DATE,
    delist_date      DATE,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE tms.tickers IS
    'Sharadar TICKERS universe master. Row filter (survivorship policy, spec §2.5): table=SF1 AND category startswith ''Domestic Common Stock'' (active AND delisted kept); table=SFP AND isdelisted=N (active only). Full overwrite per sync.';
COMMENT ON COLUMN tms.tickers.is_delisted IS
    'Sharadar isdelisted: source strings ''Y''/''N'' mapped to boolean by the importer.';
COMMENT ON COLUMN tms.tickers.table_name IS
    'Sharadar column ''table'' (renamed: reserved word). SF1 = common stocks, SFP = ETFs/funds.';
COMMENT ON COLUMN tms.tickers.last_price_date IS
    'NULL = still active (source NaT or empty string, spec §2.5/§7.2).';
COMMENT ON COLUMN tms.tickers.delist_date IS
    'Sharadar keep-column ''delistedate'' — never present in real API output (spec Q2); kept for completeness, expected NULL.';

-- Tradability-window queries (_filter_by_window, spec §7.2): keep iff
-- (first_price_date IS NULL OR first_price_date <= end) AND
-- (last_price_date IS NULL OR last_price_date >= start).
CREATE INDEX tickers_window_idx ON tms.tickers (table_name, first_price_date, last_price_date);

CREATE TRIGGER tickers_set_updated_at
    BEFORE UPDATE ON tms.tickers
    FOR EACH ROW EXECUTE FUNCTION tms.set_updated_at();

-- ---------------------------------------------------------------------------
-- bars_daily — SEP (stocks) + SFP (ETFs/funds) daily bars in one hypertable,
-- discriminated by source. A ticker lives in exactly one source; the reader
-- must not assume it (spec §7.5).
-- ---------------------------------------------------------------------------
CREATE TABLE tms.bars_daily (
    ticker       TEXT        NOT NULL CHECK (ticker <> ''),
    ts           TIMESTAMPTZ NOT NULL,
    source       TEXT        NOT NULL CHECK (source IN ('SEP', 'SFP')),
    open         BIGINT,
    high         BIGINT,
    low          BIGINT,
    close        BIGINT,
    volume       BIGINT      CHECK (volume IS NULL OR volume >= 0),
    close_adj    BIGINT,
    close_unadj  BIGINT,
    dividends    BIGINT,
    last_updated DATE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (ticker, ts, source)
);

COMMENT ON TABLE tms.bars_daily IS
    'Sharadar SEP/SFP daily bars. ts = trading date at UTC midnight. Merge semantics (spec §6): dedup key (ticker, date) per source, new rows win (revised bars replace).';
COMMENT ON COLUMN tms.bars_daily.open IS
    'Split-adjusted open, USD fixed-point 1e-4 (stored = dollars * 10000). NULL = source NaN.';
COMMENT ON COLUMN tms.bars_daily.high IS 'Split-adjusted high, USD 1e-4 fixed-point. NULL = source NaN.';
COMMENT ON COLUMN tms.bars_daily.low IS 'Split-adjusted low, USD 1e-4 fixed-point. NULL = source NaN.';
COMMENT ON COLUMN tms.bars_daily.close IS 'Split-adjusted close, USD 1e-4 fixed-point. NULL = source NaN.';
COMMENT ON COLUMN tms.bars_daily.volume IS
    'Share volume. Source is double with NaN rows upstream (spec §2.1); NaN maps to NULL, consumers decide.';
COMMENT ON COLUMN tms.bars_daily.close_adj IS
    'Fully adjusted close (splits + dividends), USD 1e-4 fixed-point. Stored, never consumed (spec §2.1 completeness).';
COMMENT ON COLUMN tms.bars_daily.close_unadj IS 'Raw unadjusted close, USD 1e-4 fixed-point. Stored, never consumed.';
COMMENT ON COLUMN tms.bars_daily.dividends IS
    'USD 1e-4 fixed-point. Sharadar documents this SEP column but the production cache predates it (spec Q7); nullable superset tolerance.';
COMMENT ON COLUMN tms.bars_daily.last_updated IS 'Sharadar row revision date (lastupdated).';

SELECT create_hypertable('tms.bars_daily', 'ts', chunk_time_interval => INTERVAL '1 year');

ALTER TABLE tms.bars_daily SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'ticker',
    timescaledb.compress_orderby   = 'ts ASC, source ASC'
);
SELECT add_compression_policy('tms.bars_daily', INTERVAL '400 days');

-- ---------------------------------------------------------------------------
-- bars_intraday — intraday bars (e.g. SPY 1-MINUTE heartbeat, moomoo feeds).
-- ---------------------------------------------------------------------------
CREATE TABLE tms.bars_intraday (
    ticker      TEXT        NOT NULL CHECK (ticker <> ''),
    ts          TIMESTAMPTZ NOT NULL,
    bar_seconds INTEGER     NOT NULL DEFAULT 60 CHECK (bar_seconds > 0),
    open        BIGINT      NOT NULL,
    high        BIGINT      NOT NULL,
    low         BIGINT      NOT NULL,
    close       BIGINT      NOT NULL,
    volume      BIGINT      NOT NULL DEFAULT 0 CHECK (volume >= 0),
    vwap        BIGINT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (ticker, ts, bar_seconds),
    CHECK (high >= low)
);

COMMENT ON TABLE tms.bars_intraday IS
    'Intraday OHLCV bars. ts = bar open time (UTC). bar_seconds = bar width (60 = 1-minute). Prices USD fixed-point 1e-4 (stored = dollars * 10000).';

SELECT create_hypertable('tms.bars_intraday', 'ts', chunk_time_interval => INTERVAL '7 days');

ALTER TABLE tms.bars_intraday SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'ticker',
    timescaledb.compress_orderby   = 'ts ASC, bar_seconds ASC'
);
SELECT add_compression_policy('tms.bars_intraday', INTERVAL '30 days');

-- ---------------------------------------------------------------------------
-- fundamentals_sf1 — SHARADAR/SF1 quarterly fundamentals, all 6 dimensions
-- cached verbatim (spec §2.3): 7 key columns + 105 metric columns exactly as
-- observed in the production cache parquet (spec lists the same set; its
-- "104" prose count is off by one — the on-disk schema is authoritative).
-- Metric columns stay DOUBLE PRECISION: they are analytics inputs consumed
-- via Decimal(str(float)) in the reference (spec §2.3, §10), not Money-path
-- prices.
-- ---------------------------------------------------------------------------
CREATE TABLE tms.fundamentals_sf1 (
    ticker        TEXT NOT NULL CHECK (ticker <> ''),
    dimension     TEXT NOT NULL CHECK (dimension IN ('ARQ', 'ART', 'ARY', 'MRQ', 'MRT', 'MRY')),
    calendardate  DATE,
    datekey       DATE NOT NULL,
    reportperiod  DATE,
    fiscalperiod  TEXT,
    lastupdated   DATE,
    accoci                   DOUBLE PRECISION,
    assets                   DOUBLE PRECISION,
    assetsavg                DOUBLE PRECISION,
    assetsc                  DOUBLE PRECISION,
    assetsnc                 DOUBLE PRECISION,
    assetturnover            DOUBLE PRECISION,
    bvps                     DOUBLE PRECISION,
    capex                    DOUBLE PRECISION,
    cashneq                  DOUBLE PRECISION,
    cashnequsd               DOUBLE PRECISION,
    cor                      DOUBLE PRECISION,
    consolinc                DOUBLE PRECISION,
    currentratio             DOUBLE PRECISION,
    de                       DOUBLE PRECISION,
    debt                     DOUBLE PRECISION,
    debtc                    DOUBLE PRECISION,
    debtnc                   DOUBLE PRECISION,
    debtusd                  DOUBLE PRECISION,
    deferredrev              DOUBLE PRECISION,
    depamor                  DOUBLE PRECISION,
    deposits                 DOUBLE PRECISION,
    divyield                 DOUBLE PRECISION,
    dps                      DOUBLE PRECISION,
    ebit                     DOUBLE PRECISION,
    ebitda                   DOUBLE PRECISION,
    ebitdamargin             DOUBLE PRECISION,
    ebitdausd                DOUBLE PRECISION,
    ebitusd                  DOUBLE PRECISION,
    ebt                      DOUBLE PRECISION,
    eps                      DOUBLE PRECISION,
    epsdil                   DOUBLE PRECISION,
    epsusd                   DOUBLE PRECISION,
    equity                   DOUBLE PRECISION,
    equityavg                DOUBLE PRECISION,
    equityusd                DOUBLE PRECISION,
    ev                       DOUBLE PRECISION,
    evebit                   DOUBLE PRECISION,
    evebitda                 DOUBLE PRECISION,
    fcf                      DOUBLE PRECISION,
    fcfps                    DOUBLE PRECISION,
    fxusd                    DOUBLE PRECISION,
    gp                       DOUBLE PRECISION,
    grossmargin              DOUBLE PRECISION,
    intangibles              DOUBLE PRECISION,
    intexp                   DOUBLE PRECISION,
    invcap                   DOUBLE PRECISION,
    invcapavg                DOUBLE PRECISION,
    inventory                DOUBLE PRECISION,
    investments              DOUBLE PRECISION,
    investmentsc             DOUBLE PRECISION,
    investmentsnc            DOUBLE PRECISION,
    liabilities              DOUBLE PRECISION,
    liabilitiesc             DOUBLE PRECISION,
    liabilitiesnc            DOUBLE PRECISION,
    marketcap                DOUBLE PRECISION,
    ncf                      DOUBLE PRECISION,
    ncfbus                   DOUBLE PRECISION,
    ncfcommon                DOUBLE PRECISION,
    ncfdebt                  DOUBLE PRECISION,
    ncfdiv                   DOUBLE PRECISION,
    ncff                     DOUBLE PRECISION,
    ncfi                     DOUBLE PRECISION,
    ncfinv                   DOUBLE PRECISION,
    ncfo                     DOUBLE PRECISION,
    ncfx                     DOUBLE PRECISION,
    netinc                   DOUBLE PRECISION,
    netinccmn                DOUBLE PRECISION,
    netinccmnusd             DOUBLE PRECISION,
    netincdis                DOUBLE PRECISION,
    netincnci                DOUBLE PRECISION,
    netmargin                DOUBLE PRECISION,
    opex                     DOUBLE PRECISION,
    opinc                    DOUBLE PRECISION,
    payables                 DOUBLE PRECISION,
    payoutratio              DOUBLE PRECISION,
    pb                       DOUBLE PRECISION,
    pe                       DOUBLE PRECISION,
    pe1                      DOUBLE PRECISION,
    ppnenet                  DOUBLE PRECISION,
    prefdivis                DOUBLE PRECISION,
    price                    DOUBLE PRECISION,
    ps                       DOUBLE PRECISION,
    ps1                      DOUBLE PRECISION,
    receivables              DOUBLE PRECISION,
    retearn                  DOUBLE PRECISION,
    revenue                  DOUBLE PRECISION,
    revenueusd               DOUBLE PRECISION,
    rnd                      DOUBLE PRECISION,
    roa                      DOUBLE PRECISION,
    roe                      DOUBLE PRECISION,
    roic                     DOUBLE PRECISION,
    ros                      DOUBLE PRECISION,
    sbcomp                   DOUBLE PRECISION,
    sgna                     DOUBLE PRECISION,
    sharefactor              DOUBLE PRECISION,
    sharesbas                DOUBLE PRECISION,
    shareswa                 DOUBLE PRECISION,
    shareswadil              DOUBLE PRECISION,
    sps                      DOUBLE PRECISION,
    tangibles                DOUBLE PRECISION,
    taxassets                DOUBLE PRECISION,
    taxexp                   DOUBLE PRECISION,
    taxliabilities           DOUBLE PRECISION,
    tbvps                    DOUBLE PRECISION,
    workingcapital           DOUBLE PRECISION,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (ticker, datekey, dimension)
);

COMMENT ON TABLE tms.fundamentals_sf1 IS
    'Sharadar SF1 fundamentals, all six dimensions coexist per (ticker, datekey); dedup/PK = (ticker, datekey, dimension) (spec §2.3). datekey = filing date (point-in-time key).';
COMMENT ON COLUMN tms.fundamentals_sf1.marketcap IS
    'Raw USD (e.g. 3.4e12 for AAPL); consumers compare > 0 and convert via shortest-repr decimal string (spec §2.3).';

-- _load_sf1_mrt / load_sf1_market_caps scan dimension=MRT with datekey <= as_of.
CREATE INDEX fundamentals_sf1_dim_datekey_idx ON tms.fundamentals_sf1 (dimension, datekey);

CREATE TRIGGER fundamentals_sf1_set_updated_at
    BEFORE UPDATE ON tms.fundamentals_sf1
    FOR EACH ROW EXECUTE FUNCTION tms.set_updated_at();

-- ---------------------------------------------------------------------------
-- events — SHARADAR/EVENTS corporate events.
-- ---------------------------------------------------------------------------
CREATE TABLE tms.events (
    ticker     TEXT        NOT NULL CHECK (ticker <> ''),
    event_date DATE        NOT NULL,
    eventcodes TEXT        NOT NULL CHECK (eventcodes <> ''),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (ticker, event_date, eventcodes)
);

COMMENT ON TABLE tms.events IS
    'Sharadar EVENTS. eventcodes = pipe-separated numeric codes (e.g. ''22|71''). Earnings filter: ''22'' must be an exact |-split member, never substring (spec §2.4). Dedup key (ticker, date, eventcodes): same-day events with different code strings coexist.';

CREATE INDEX events_date_idx ON tms.events (event_date);

CREATE TRIGGER events_set_updated_at
    BEFORE UPDATE ON tms.events
    FOR EACH ROW EXECUTE FUNCTION tms.set_updated_at();

-- ---------------------------------------------------------------------------
-- universe_snapshots — frozen ticker universes (live top-N by market cap,
-- EOD refresh, backtest windows) for reproducibility and audit (spec §10:
-- list_universe_for_window + exclusions + market-cap cap).
-- ---------------------------------------------------------------------------
CREATE TABLE tms.universe_snapshots (
    id           BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    as_of        DATE        NOT NULL,
    kind         TEXT        NOT NULL CHECK (kind IN ('live', 'eod', 'backtest', 'manual')),
    table_filter TEXT        CHECK (table_filter IS NULL OR table_filter IN ('SF1', 'SFP')),
    window_start DATE,
    window_end   DATE        CHECK (window_end IS NULL OR window_start IS NULL OR window_end >= window_start),
    limit_n      INTEGER     CHECK (limit_n IS NULL OR limit_n > 0),
    tickers      TEXT[]      NOT NULL,
    excluded     TEXT[]      NOT NULL DEFAULT '{}',
    params       JSONB       NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(params) = 'object'),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE tms.universe_snapshots IS
    'Immutable record of each computed universe: inputs (window, table filter, exclusions, top-N limit) + resulting ordered ticker list. Append-only.';
COMMENT ON COLUMN tms.universe_snapshots.limit_n IS
    'Top-N cap by market cap desc (live default 85, TMS_LIVE_UNIVERSE_LIMIT); NULL = uncapped.';

CREATE INDEX universe_snapshots_kind_asof_idx ON tms.universe_snapshots (kind, as_of DESC);

-- ---------------------------------------------------------------------------
-- dataset_sync — DB counterpart of the parquet cache''s .meta.json
-- (spec §5): last wall-clock sync time + cumulative row count per dataset.
-- ---------------------------------------------------------------------------
CREATE TABLE tms.dataset_sync (
    dataset        TEXT        NOT NULL PRIMARY KEY
                               CHECK (dataset IN ('TICKERS', 'SEP', 'SFP', 'SF1', 'EVENTS')),
    last_sync      TIMESTAMPTZ,
    row_count      BIGINT      NOT NULL DEFAULT 0 CHECK (row_count >= 0),
    schema_version INTEGER     NOT NULL DEFAULT 1,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE tms.dataset_sync IS
    'Sync bookkeeping per Sharadar dataset, mirroring CacheMeta semantics (spec §5): last_sync = wall-clock time of the sync (NOT data as-of date; catchup trusts a same-day timestamp); row_count = bootstrap rows or previous + net-new for updates (TICKERS: full rewrite count). Not re-derived from disk.';

CREATE TRIGGER dataset_sync_set_updated_at
    BEFORE UPDATE ON tms.dataset_sync
    FOR EACH ROW EXECUTE FUNCTION tms.set_updated_at();
