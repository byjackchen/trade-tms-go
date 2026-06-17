-- 000011_strategy_state: per-strategy SG state_dict persistence for crash
-- recovery (P6 locked decision 6).
--
-- On every change the paper/live trade session snapshots each strategy's
-- SignalGenerator state_dict and upserts it here keyed by
-- (trader_id, strategy_id). On restart the node restores each strategy's
-- state from the latest row, restores positions from the broker
-- (Trd_GetPositionList) and runs a reconciliation — resuming cleanly with
-- identical subsequent behaviour. This closes the cold-start gap.

CREATE TABLE tms.strategy_state (
    id          BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    trader_id   TEXT        NOT NULL CHECK (trader_id <> ''),
    strategy_id TEXT        NOT NULL CHECK (strategy_id <> ''),
    session_id  BIGINT      REFERENCES tms.sessions (id) ON DELETE SET NULL,
    state       JSONB       NOT NULL DEFAULT '{}'::jsonb,
    generation  BIGINT      NOT NULL DEFAULT 0 CHECK (generation >= 0),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (trader_id, strategy_id)
);

COMMENT ON TABLE tms.strategy_state IS
    'Per-strategy SG state_dict snapshots for crash recovery (P6 decision 6). Latest state per (trader_id, strategy_id); restored on node restart so the SignalGenerator resumes warm rather than cold. generation bumps on each save (monotonic change counter).';

CREATE INDEX strategy_state_trader_idx ON tms.strategy_state (trader_id, updated_at DESC);

CREATE TRIGGER strategy_state_set_updated_at
    BEFORE UPDATE ON tms.strategy_state
    FOR EACH ROW EXECUTE FUNCTION tms.set_updated_at();
