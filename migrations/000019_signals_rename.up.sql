-- 000019_signals_rename: term realignment of concept A (the SignalIntent family
-- — per-symbol state/strength/proximity/grade/z-score JUDGMENT snapshots with NO
-- executable side+qty) from "intent" to "signal". See
-- docs/design/intent-to-signal-rename.md §3.1 / Phase 2.
--
-- One-shot migration, NO compatibility window / NO alias / NO dual-write (Phase 2
-- locked decision 2026-06-17): rename the table, the JSONB payload column, and
-- every signal_intents_* index in place. All ALTER ... RENAME operations preserve
-- data and history (rows ride along) and are exactly reversed in the .down.sql.

-- ---------------------------------------------------------------------------
-- Table: tms.signal_intents -> tms.signals.
-- ---------------------------------------------------------------------------
ALTER TABLE tms.signal_intents RENAME TO signals;

-- ---------------------------------------------------------------------------
-- Column: the unwrapped per-strategy variant payload intent -> signal.
-- ---------------------------------------------------------------------------
ALTER TABLE tms.signals RENAME COLUMN intent TO signal;

-- ---------------------------------------------------------------------------
-- Indexes: signal_intents_* -> signals_* (000005 lookup/ts + 000010 EOD
-- idempotency unique index + as_of index + the auto-named primary key, which
-- RENAME TABLE does not rename).
-- ---------------------------------------------------------------------------
ALTER INDEX tms.signal_intents_pkey         RENAME TO signals_pkey;
ALTER INDEX tms.signal_intents_lookup_idx   RENAME TO signals_lookup_idx;
ALTER INDEX tms.signal_intents_ts_idx       RENAME TO signals_ts_idx;
ALTER INDEX tms.signal_intents_eod_idem_idx RENAME TO signals_eod_idem_idx;
ALTER INDEX tms.signal_intents_as_of_idx    RENAME TO signals_as_of_idx;
