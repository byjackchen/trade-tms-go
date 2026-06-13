-- Rollback of 000002_marketdata.
DROP TABLE IF EXISTS tms.dataset_sync;
DROP TABLE IF EXISTS tms.universe_snapshots;
DROP TABLE IF EXISTS tms.events;
DROP TABLE IF EXISTS tms.fundamentals_sf1;
DROP TABLE IF EXISTS tms.bars_intraday;
DROP TABLE IF EXISTS tms.bars_daily;
DROP TABLE IF EXISTS tms.tickers;
DROP FUNCTION IF EXISTS tms.set_updated_at();
