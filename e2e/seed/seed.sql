-- e2e deterministic seed.
--
-- Purpose: give the Data workspace meaningful, *fixed* content so the e2e
-- suite can assert UI numbers == DB numbers and render coverage / gaps / sync
-- history without depending on a real Sharadar parquet cache (which the gate
-- may not have). It is idempotent: every statement upserts or is guarded, so
-- re-running over an already-seeded DB is a no-op and never duplicates rows.
--
-- It seeds ONLY market-data + sync bookkeeping tables. It never touches
-- tms.jobs (the refresh/cancel tests create those live through the API) so the
-- job-count assertions stay honest.
--
-- Trading dates: all bar timestamps are UTC-midnight on real 2024 NYSE
-- sessions. GAPPY deliberately omits two interior sessions (2024-06-05 and
-- 2024-06-07) so the gap heatmap + coverage "gaps present" badge light up;
-- CLEAN has every session in its span.

BEGIN;

-- ---------------------------------------------------------------------------
-- tickers — three deterministic symbols (one delisted, for variety).
-- ---------------------------------------------------------------------------
INSERT INTO tms.tickers
  (ticker, name, exchange, is_delisted, category, sector, industry,
   table_name, first_price_date, last_price_date, delist_date)
VALUES
  ('CLEAN', 'Clean Coverage Co',  'NASDAQ', FALSE,
   'Domestic Common Stock', 'Technology', 'Software',
   'SF1', DATE '2024-06-03', DATE '2024-06-12', NULL),
  ('GAPPY', 'Gappy Sessions Inc', 'NYSE',   FALSE,
   'Domestic Common Stock', 'Industrials', 'Machinery',
   'SF1', DATE '2024-06-03', DATE '2024-06-12', NULL),
  ('DELIS', 'Delisted Example',   'NYSE',   TRUE,
   'Domestic Common Stock', 'Financials', 'Banks',
   'SF1', DATE '2024-01-02', DATE '2024-03-01', DATE '2024-03-01')
ON CONFLICT (ticker) DO UPDATE SET
  name             = EXCLUDED.name,
  exchange         = EXCLUDED.exchange,
  is_delisted      = EXCLUDED.is_delisted,
  category         = EXCLUDED.category,
  sector           = EXCLUDED.sector,
  industry         = EXCLUDED.industry,
  table_name       = EXCLUDED.table_name,
  first_price_date = EXCLUDED.first_price_date,
  last_price_date  = EXCLUDED.last_price_date,
  delist_date      = EXCLUDED.delist_date;

-- ---------------------------------------------------------------------------
-- bars_daily — CLEAN: every session 2024-06-03..2024-06-12.
--              GAPPY: same span but missing 2024-06-05 and 2024-06-07.
-- Prices are USD fixed-point 1e-4 (stored = dollars * 10000); the values are
-- arbitrary but valid. ts = UTC midnight on the trading date.
-- ---------------------------------------------------------------------------
INSERT INTO tms.bars_daily (ticker, ts, source, open, high, low, close, volume)
SELECT 'CLEAN', d::timestamptz, 'SEP',
       1000000, 1010000, 990000, 1005000, 1000000
FROM unnest(ARRAY[
       TIMESTAMPTZ '2024-06-03 00:00:00+00',
       TIMESTAMPTZ '2024-06-04 00:00:00+00',
       TIMESTAMPTZ '2024-06-05 00:00:00+00',
       TIMESTAMPTZ '2024-06-06 00:00:00+00',
       TIMESTAMPTZ '2024-06-07 00:00:00+00',
       TIMESTAMPTZ '2024-06-10 00:00:00+00',
       TIMESTAMPTZ '2024-06-11 00:00:00+00',
       TIMESTAMPTZ '2024-06-12 00:00:00+00'
     ]) AS d
ON CONFLICT (ticker, ts, source) DO NOTHING;

INSERT INTO tms.bars_daily (ticker, ts, source, open, high, low, close, volume)
SELECT 'GAPPY', d::timestamptz, 'SEP',
       2000000, 2020000, 1980000, 2010000, 500000
FROM unnest(ARRAY[
       TIMESTAMPTZ '2024-06-03 00:00:00+00',
       TIMESTAMPTZ '2024-06-04 00:00:00+00',
       -- 2024-06-05 omitted (gap)
       TIMESTAMPTZ '2024-06-06 00:00:00+00',
       -- 2024-06-07 omitted (gap)
       TIMESTAMPTZ '2024-06-10 00:00:00+00',
       TIMESTAMPTZ '2024-06-11 00:00:00+00',
       TIMESTAMPTZ '2024-06-12 00:00:00+00'
     ]) AS d
ON CONFLICT (ticker, ts, source) DO NOTHING;

-- ---------------------------------------------------------------------------
-- MANUAL trading-desk symbols (AAPL/TSLA/MSFT) — real-world-priced bars for the
-- manual-desk specs (32-38) and the mock OpenD venue. The mock venue prices +
-- fills manual orders off the LATEST bars_daily.close, and the manual desk's
-- risk-gate brokerPriceSource reads the SAME latest close, so these prices flow
-- into BOTH the fill economics AND the budget/concentration gate.
--
-- SAFETY (CRITICAL — finding 1, the risk-gate blocker): close is USD fixed-point
-- 1e-4 (stored = dollars * 10000), IDENTICAL to the CLEAN/GAPPY blocks above and
-- to the domain.Price.Raw() convention the mock venue (mock/source.go:
-- `Close: domain.Price(c)`) + every price reader decode. The prior dev fixture
-- seeded these as the BARE dollar value (190/250/420), i.e. 1e4 TOO SMALL, so the
-- mock venue filled AAPL at $0.019 and the gate priced a 10,000-share order at
-- ~$190 (< the $100k MANUAL budget) and SILENTLY APPROVED a 25x-NAV market order.
-- Correctly scaled ($190 -> 1_900_000), a 10,000-share AAPL/TSLA/MSFT market order
-- is $1.9M/$2.5M/$4.2M and the allocator budget gate REJECTS it (HTTP 422) exactly
-- as the identical-notional LIMIT order does — the gate binds on MARKET orders.
--
-- AAPL $190, TSLA $250, MSFT $420 (whole-dollar OHLC; close is what the venue
-- prices off). NVDA is deliberately LEFT UNPRICED so the "unpriced symbol fails
-- the gate CLOSED" path (risk.unpriced_symbol -> 422) stays exercised.
INSERT INTO tms.bars_daily (ticker, ts, source, open, high, low, close, volume)
SELECT s.ticker, d::timestamptz, 'SEP',
       s.px, s.px, s.px, s.px, 1000000
FROM unnest(ARRAY[
       TIMESTAMPTZ '2024-06-03 00:00:00+00',
       TIMESTAMPTZ '2024-06-04 00:00:00+00',
       TIMESTAMPTZ '2024-06-05 00:00:00+00',
       TIMESTAMPTZ '2024-06-06 00:00:00+00',
       TIMESTAMPTZ '2024-06-07 00:00:00+00',
       TIMESTAMPTZ '2024-06-10 00:00:00+00',
       TIMESTAMPTZ '2024-06-11 00:00:00+00',
       TIMESTAMPTZ '2024-06-12 00:00:00+00'
     ]) AS d
CROSS JOIN (VALUES
       ('AAPL', 1900000::bigint),  -- $190.00
       ('TSLA', 2500000::bigint),  -- $250.00
       ('MSFT', 4200000::bigint)   -- $420.00
     ) AS s(ticker, px)
ON CONFLICT (ticker, ts, source) DO UPDATE SET
  open   = EXCLUDED.open,
  high   = EXCLUDED.high,
  low    = EXCLUDED.low,
  close  = EXCLUDED.close,
  volume = EXCLUDED.volume;

-- ---------------------------------------------------------------------------
-- fundamentals_sf1 — one quarterly row per ticker (just enough for a non-zero
-- coverage count). All metric columns default NULL.
-- ---------------------------------------------------------------------------
INSERT INTO tms.fundamentals_sf1
  (ticker, dimension, calendardate, datekey, reportperiod, marketcap)
VALUES
  ('CLEAN', 'MRT', DATE '2024-03-31', DATE '2024-04-30', DATE '2024-03-31', 5.0e10),
  ('GAPPY', 'MRT', DATE '2024-03-31', DATE '2024-04-30', DATE '2024-03-31', 1.2e10)
ON CONFLICT (ticker, datekey, dimension) DO NOTHING;

-- ---------------------------------------------------------------------------
-- events — one corporate event per ticker (earnings code "22").
-- ---------------------------------------------------------------------------
INSERT INTO tms.events (ticker, event_date, eventcodes)
VALUES
  ('CLEAN', DATE '2024-04-30', '22'),
  ('GAPPY', DATE '2024-04-30', '22|71')
ON CONFLICT (ticker, event_date, eventcodes) DO NOTHING;

-- ---------------------------------------------------------------------------
-- dataset_sync — watermarks per dataset (drives the sync "Watermarks" table).
-- ---------------------------------------------------------------------------
INSERT INTO tms.dataset_sync (dataset, last_sync, row_count, schema_version)
VALUES
  ('TICKERS', TIMESTAMPTZ '2024-06-12 22:00:00+00', 3,  1),
  ('SEP',     TIMESTAMPTZ '2024-06-12 22:05:00+00', 14, 1),
  ('SF1',     TIMESTAMPTZ '2024-06-12 22:06:00+00', 2,  1),
  ('EVENTS',  TIMESTAMPTZ '2024-06-12 22:07:00+00', 2,  1)
ON CONFLICT (dataset) DO UPDATE SET
  last_sync      = EXCLUDED.last_sync,
  row_count      = EXCLUDED.row_count,
  schema_version = EXCLUDED.schema_version;

-- ---------------------------------------------------------------------------
-- dataset_sync_runs — at least one historical import run so the "Run history"
-- table is populated. Guarded so re-seeding never appends duplicates: only
-- insert when this exact seed marker run is absent.
-- ---------------------------------------------------------------------------
INSERT INTO tms.dataset_sync_runs
  (dataset, kind, started_at, finished_at, rows_added, status, error)
SELECT 'SEP', 'import',
       TIMESTAMPTZ '2024-06-12 22:00:00+00',
       TIMESTAMPTZ '2024-06-12 22:05:00+00',
       14, 'ok', NULL
WHERE NOT EXISTS (
  SELECT 1 FROM tms.dataset_sync_runs
  WHERE dataset = 'SEP'
    AND started_at = TIMESTAMPTZ '2024-06-12 22:00:00+00'
);

INSERT INTO tms.dataset_sync_runs
  (dataset, kind, started_at, finished_at, rows_added, status, error)
SELECT 'TICKERS', 'import',
       TIMESTAMPTZ '2024-06-12 21:59:00+00',
       TIMESTAMPTZ '2024-06-12 22:00:00+00',
       3, 'ok', NULL
WHERE NOT EXISTS (
  SELECT 1 FROM tms.dataset_sync_runs
  WHERE dataset = 'TICKERS'
    AND started_at = TIMESTAMPTZ '2024-06-12 21:59:00+00'
);

COMMIT;
