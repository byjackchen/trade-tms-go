-- Revert 000010_eod_idempotency.
DROP INDEX IF EXISTS tms.signal_intents_as_of_idx;
DROP INDEX IF EXISTS tms.signal_intents_eod_idem_idx;
ALTER TABLE tms.signal_intents DROP COLUMN IF EXISTS as_of;
