-- 000019_signals_rename (down): reverse every rename, restoring the
-- "signal_intents" identifiers from 000005_live / 000010_eod_idempotency exactly.

-- ---------------------------------------------------------------------------
-- Indexes.
-- ---------------------------------------------------------------------------
ALTER INDEX tms.signals_as_of_idx    RENAME TO signal_intents_as_of_idx;
ALTER INDEX tms.signals_eod_idem_idx RENAME TO signal_intents_eod_idem_idx;
ALTER INDEX tms.signals_ts_idx       RENAME TO signal_intents_ts_idx;
ALTER INDEX tms.signals_lookup_idx   RENAME TO signal_intents_lookup_idx;
ALTER INDEX tms.signals_pkey         RENAME TO signal_intents_pkey;

-- ---------------------------------------------------------------------------
-- Column.
-- ---------------------------------------------------------------------------
ALTER TABLE tms.signals RENAME COLUMN signal TO intent;

-- ---------------------------------------------------------------------------
-- Table.
-- ---------------------------------------------------------------------------
ALTER TABLE tms.signals RENAME TO signal_intents;
