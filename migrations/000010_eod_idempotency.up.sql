-- 000010_eod_idempotency: make EOD refresh an idempotent UPSERT target.
--
-- P5 locked decision 4: the EOD engine-REPLAY mode must be idempotent — running
-- `tms eod --as-of <date>` twice produces the SAME live.signal_intents rows, not
-- duplicates. Rather than a non-idempotent side path that appends, the EOD
-- mode replays bars through the SAME engine and UPSERTs on
-- (strategy_id, symbol, as_of).
--
-- The streaming live path (tms.signal_intents 000005) is append-only by design
-- (one row per evaluate_intent per bar, the SignalIntentUpdate audit trail), so
-- the idempotency key must NOT constrain it. We add a nullable as_of DATE that
-- is set ONLY by the EOD writer and a PARTIAL UNIQUE index keyed on
-- (strategy_id, symbol, as_of) WHERE as_of IS NOT NULL. Streaming rows leave
-- as_of NULL and remain unconstrained (append-only); EOD rows carry as_of and
-- collide-then-overwrite on re-run.

ALTER TABLE tms.signal_intents
    ADD COLUMN as_of DATE;

COMMENT ON COLUMN tms.signal_intents.as_of IS
    'EOD refresh as-of trading date (P5 decision 4). NULL for the append-only streaming live path; set by the EOD engine-replay writer, which UPSERTs idempotently on (strategy_id, symbol, as_of) so a re-run overwrites rather than duplicates.';

-- Idempotency target for the EOD UPSERT. Partial (as_of IS NOT NULL) so the
-- streaming append-only path is unaffected.
CREATE UNIQUE INDEX signal_intents_eod_idem_idx
    ON tms.signal_intents (strategy_id, symbol, as_of)
    WHERE as_of IS NOT NULL;

-- EOD readers query the latest refresh for a (strategy_id, as_of) cheaply.
CREATE INDEX signal_intents_as_of_idx
    ON tms.signal_intents (as_of DESC, strategy_id)
    WHERE as_of IS NOT NULL;
