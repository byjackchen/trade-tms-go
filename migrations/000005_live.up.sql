-- 000005_live: live trading domain.
--
-- Durable system-of-record for live/paper/signal sessions: order + fill +
-- position books, signal intents, risk-gate decisions, halts and EOD
-- reconciliation reports (docs/spec/portfolio-risk.md, api-ws-redis.md §2/§5).
-- Redis remains the hot mirror for UI fan-out; this schema is the audit/
-- recovery store. Money columns are BIGINT fixed-point 1e-4 USD
-- (stored = dollars * 10000); event-precision timestamps from the engine are
-- int64 ns UTC and stored as TIMESTAMPTZ (microsecond precision suffices for
-- persistence; the ns originals live in the JSONB detail blobs).

-- ---------------------------------------------------------------------------
-- sessions — one row per trading-node run.
-- ---------------------------------------------------------------------------
CREATE TABLE tms.sessions (
    id         BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    trader_id  TEXT        NOT NULL CHECK (trader_id <> ''),
    mode       TEXT        NOT NULL CHECK (mode IN ('signal', 'paper', 'live')),
    status     TEXT        NOT NULL DEFAULT 'RUNNING' CHECK (status IN ('RUNNING', 'STOPPED', 'CRASHED')),
    started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    ended_at   TIMESTAMPTZ CHECK (ended_at IS NULL OR ended_at >= started_at),
    config     JSONB       NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(config) = 'object'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((status = 'RUNNING') = (ended_at IS NULL))
);

COMMENT ON TABLE tms.sessions IS
    'Trading-node sessions. trader_id = Redis namespace id (e.g. PAPER-SMOKE-001, api spec §1.3). mode: signal = no account, paper = SIMULATE, live = real.';

CREATE INDEX sessions_trader_started_idx ON tms.sessions (trader_id, started_at DESC);
-- at most one running session per trader id
CREATE UNIQUE INDEX sessions_one_running_idx ON tms.sessions (trader_id) WHERE status = 'RUNNING';

CREATE TRIGGER sessions_set_updated_at
    BEFORE UPDATE ON tms.sessions
    FOR EACH ROW EXECUTE FUNCTION tms.set_updated_at();

-- ---------------------------------------------------------------------------
-- orders — latest order state (lifecycle event history in fills + details).
-- ---------------------------------------------------------------------------
CREATE TABLE tms.orders (
    id              BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    session_id      BIGINT      NOT NULL REFERENCES tms.sessions (id) ON DELETE RESTRICT,
    client_order_id TEXT        NOT NULL UNIQUE CHECK (client_order_id <> ''),
    venue_order_id  TEXT,
    strategy_id     TEXT        NOT NULL CHECK (strategy_id <> ''),
    symbol          TEXT        NOT NULL CHECK (symbol <> ''),
    instrument_id   TEXT        NOT NULL CHECK (instrument_id <> ''),
    side            TEXT        NOT NULL CHECK (side IN ('BUY', 'SELL')),
    order_type      TEXT        NOT NULL CHECK (order_type IN ('MARKET', 'LIMIT', 'STOP_MARKET', 'STOP_LIMIT')),
    qty             BIGINT      NOT NULL CHECK (qty > 0),
    limit_px        BIGINT      CHECK (limit_px IS NULL OR limit_px > 0),
    stop_px         BIGINT      CHECK (stop_px IS NULL OR stop_px > 0),
    tif             TEXT        NOT NULL DEFAULT 'DAY' CHECK (tif IN ('DAY', 'GTC', 'IOC', 'FOK', 'GTD')),
    status          TEXT        NOT NULL DEFAULT 'INITIALIZED'
                                CHECK (status IN ('INITIALIZED', 'DENIED', 'SUBMITTED', 'ACCEPTED',
                                                  'REJECTED', 'PENDING_UPDATE', 'PENDING_CANCEL',
                                                  'CANCELED', 'EXPIRED', 'TRIGGERED',
                                                  'PARTIALLY_FILLED', 'FILLED')),
    filled_qty      BIGINT      NOT NULL DEFAULT 0 CHECK (filled_qty >= 0 AND filled_qty <= qty),
    avg_fill_px     BIGINT      CHECK (avg_fill_px IS NULL OR avg_fill_px > 0),
    reason          TEXT,
    ts_submitted    TIMESTAMPTZ,
    ts_last_event   TIMESTAMPTZ,
    details         JSONB       NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(details) = 'object'),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (order_type NOT IN ('LIMIT', 'STOP_LIMIT') OR limit_px IS NOT NULL),
    CHECK (order_type NOT IN ('STOP_MARKET', 'STOP_LIMIT') OR stop_px IS NOT NULL),
    CHECK (status <> 'FILLED' OR filled_qty = qty)
);

COMMENT ON TABLE tms.orders IS
    'Live/paper orders, latest state per client_order_id (the Redis order LIST holds the full per-event history for the UI; api spec §2.1). instrument_id includes venue (AAPL.MOOMOO); symbol is the bare ticker.';
COMMENT ON COLUMN tms.orders.limit_px IS 'USD fixed-point 1e-4.';
COMMENT ON COLUMN tms.orders.stop_px IS 'USD fixed-point 1e-4.';
COMMENT ON COLUMN tms.orders.avg_fill_px IS 'USD fixed-point 1e-4, volume-weighted across fills.';
COMMENT ON COLUMN tms.orders.reason IS 'Denial/rejection/cancel reason (e.g. portfolio gate rule text).';

CREATE INDEX orders_session_idx ON tms.orders (session_id, created_at DESC);
CREATE INDEX orders_strategy_idx ON tms.orders (strategy_id, created_at DESC);
CREATE INDEX orders_symbol_idx ON tms.orders (symbol, created_at DESC);
CREATE INDEX orders_open_idx ON tms.orders (status)
    WHERE status IN ('INITIALIZED', 'SUBMITTED', 'ACCEPTED', 'PENDING_UPDATE', 'PENDING_CANCEL',
                     'TRIGGERED', 'PARTIALLY_FILLED');

CREATE TRIGGER orders_set_updated_at
    BEFORE UPDATE ON tms.orders
    FOR EACH ROW EXECUTE FUNCTION tms.set_updated_at();

-- ---------------------------------------------------------------------------
-- fills — immutable executions.
-- ---------------------------------------------------------------------------
CREATE TABLE tms.fills (
    id             BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    order_id       BIGINT      NOT NULL REFERENCES tms.orders (id) ON DELETE CASCADE,
    venue_trade_id TEXT,
    qty            BIGINT      NOT NULL CHECK (qty > 0),
    px             BIGINT      NOT NULL CHECK (px > 0),
    fee_usd        BIGINT      NOT NULL DEFAULT 0,
    liquidity      TEXT        CHECK (liquidity IS NULL OR liquidity IN ('MAKER', 'TAKER')),
    ts             TIMESTAMPTZ NOT NULL,
    details        JSONB       NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(details) = 'object'),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (order_id, venue_trade_id)
);

COMMENT ON TABLE tms.fills IS
    'Executions, append-only. px = USD fixed-point 1e-4; fee_usd = USD fixed-point 1e-4 (reference currently stamps commission 0 — money spec). details carries the raw fill event blob (events.fills.{instrument_id} payload).';

CREATE INDEX fills_order_ts_idx ON tms.fills (order_id, ts);
CREATE INDEX fills_ts_idx ON tms.fills (ts DESC);

-- ---------------------------------------------------------------------------
-- positions — latest position state per engine position id.
-- ---------------------------------------------------------------------------
CREATE TABLE tms.positions (
    id                 BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    session_id         BIGINT      NOT NULL REFERENCES tms.sessions (id) ON DELETE RESTRICT,
    position_id        TEXT        NOT NULL CHECK (position_id <> ''),
    strategy_id        TEXT        NOT NULL CHECK (strategy_id <> ''),
    symbol             TEXT        NOT NULL CHECK (symbol <> ''),
    instrument_id      TEXT        NOT NULL CHECK (instrument_id <> ''),
    signed_qty         BIGINT      NOT NULL,
    avg_entry_px       BIGINT      CHECK (avg_entry_px IS NULL OR avg_entry_px > 0),
    avg_exit_px        BIGINT      CHECK (avg_exit_px IS NULL OR avg_exit_px > 0),
    realized_pnl_usd   BIGINT      NOT NULL DEFAULT 0,
    unrealized_pnl_usd BIGINT      NOT NULL DEFAULT 0,
    status             TEXT        NOT NULL DEFAULT 'OPEN' CHECK (status IN ('OPEN', 'CLOSED')),
    opened_at          TIMESTAMPTZ NOT NULL,
    closed_at          TIMESTAMPTZ CHECK (closed_at IS NULL OR closed_at >= opened_at),
    details            JSONB       NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(details) = 'object'),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (session_id, position_id),
    CHECK ((status = 'CLOSED') = (closed_at IS NOT NULL)),
    CHECK (status <> 'CLOSED' OR signed_qty = 0)
);

COMMENT ON TABLE tms.positions IS
    'Position book, latest state per (session, engine position_id). signed_qty: + long, - short, 0 = flat/closed (portfolio spec §1.4 convention). Aggregations for risk gates and reconciliation sum signed_qty per (strategy_id, symbol), skipping zero.';
COMMENT ON COLUMN tms.positions.avg_entry_px IS 'USD fixed-point 1e-4.';
COMMENT ON COLUMN tms.positions.realized_pnl_usd IS 'USD fixed-point 1e-4.';

CREATE INDEX positions_strategy_symbol_idx ON tms.positions (strategy_id, symbol);
CREATE INDEX positions_session_open_idx ON tms.positions (session_id) WHERE status = 'OPEN';

CREATE TRIGGER positions_set_updated_at
    BEFORE UPDATE ON tms.positions
    FOR EACH ROW EXECUTE FUNCTION tms.set_updated_at();

-- ---------------------------------------------------------------------------
-- signal_intents — strategy signal snapshots (data.SignalIntentUpdate).
-- ---------------------------------------------------------------------------
CREATE TABLE tms.signal_intents (
    id          BIGINT           GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    session_id  BIGINT           REFERENCES tms.sessions (id) ON DELETE SET NULL,
    strategy_id TEXT             NOT NULL CHECK (strategy_id IN ('sepa', 'pairs', 'sector_rotation', 'intraday_breakout')),
    symbol      TEXT             NOT NULL CHECK (symbol <> ''),
    state       TEXT             NOT NULL CHECK (state IN ('no_setup', 'forming', 'buy', 'hold', 'exit', 'stop_hit')),
    strength    DOUBLE PRECISION NOT NULL CHECK (strength >= 0 AND strength <= 100),
    proximity_to_trigger_pct DOUBLE PRECISION,
    generation  BIGINT           NOT NULL CHECK (generation >= 0),
    intent      JSONB            NOT NULL CHECK (jsonb_typeof(intent) = 'object'),
    ts_event_ns BIGINT           NOT NULL CHECK (ts_event_ns >= 0),
    ts          TIMESTAMPTZ      NOT NULL,
    created_at  TIMESTAMPTZ      NOT NULL DEFAULT now()
);

COMMENT ON TABLE tms.signal_intents IS
    'Signal intent snapshots, append-only. strategy_id is the discriminator of the SignalIntentUnion (api spec §5.9); intent = the full unwrapped variant payload (per-strategy extra fields). UI dedup is (symbol, strategy_id) newest-wins.';
COMMENT ON COLUMN tms.signal_intents.ts_event_ns IS 'Engine ts_event, int64 ns since epoch UTC (exact); ts is the same instant at TIMESTAMPTZ precision for range queries.';

CREATE INDEX signal_intents_lookup_idx ON tms.signal_intents (strategy_id, symbol, ts DESC);
CREATE INDEX signal_intents_ts_idx ON tms.signal_intents (ts DESC);

-- ---------------------------------------------------------------------------
-- risk_events — every portfolio-gate decision worth auditing.
-- ---------------------------------------------------------------------------
CREATE TABLE tms.risk_events (
    id          BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    session_id  BIGINT      REFERENCES tms.sessions (id) ON DELETE SET NULL,
    rule_name   TEXT        NOT NULL CHECK (rule_name <> ''),
    approved    BOOLEAN     NOT NULL,
    strategy_id TEXT        NOT NULL CHECK (strategy_id <> ''),
    symbol      TEXT        NOT NULL CHECK (symbol <> ''),
    side        TEXT        NOT NULL CHECK (side IN ('LONG', 'SHORT', 'FLAT')),
    qty         BIGINT      NOT NULL CHECK (qty >= 0),
    price       BIGINT      NOT NULL CHECK (price >= 0),
    reason      TEXT        NOT NULL DEFAULT '',
    snapshot    JSONB       NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(snapshot) = 'object'),
    ts          TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE tms.risk_events IS
    'Portfolio gate decisions (portfolio spec §2.4/§3.2/§8.2). rule_name uses the byte-identical reference rule ids: allocator.unregistered_strategy, allocator.budget_exceeded, risk.daily_loss_halt, risk.max_single_name, risk.concentration ("" for approvals). side = SignalSide (qty unsigned, side encodes direction; FLAT bypasses all gates). price = USD fixed-point 1e-4. snapshot = AccountSnapshot inputs for replay.';

CREATE INDEX risk_events_session_ts_idx ON tms.risk_events (session_id, ts DESC);
CREATE INDEX risk_events_rejected_idx ON tms.risk_events (rule_name, ts DESC) WHERE NOT approved;

-- ---------------------------------------------------------------------------
-- halts — trading halts (daily-loss and operational).
-- ---------------------------------------------------------------------------
CREATE TABLE tms.halts (
    id           BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    session_id   BIGINT      REFERENCES tms.sessions (id) ON DELETE SET NULL,
    kind         TEXT        NOT NULL CHECK (kind IN ('daily_loss', 'manual', 'reconciliation', 'data', 'broker', 'other')),
    reason       TEXT        NOT NULL CHECK (reason <> ''),
    triggered_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    cleared_at   TIMESTAMPTZ CHECK (cleared_at IS NULL OR cleared_at >= triggered_at),
    cleared_by   TEXT,
    details      JSONB       NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(details) = 'object'),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (cleared_at IS NULL OR cleared_by IS NOT NULL)
);

COMMENT ON TABLE tms.halts IS
    'Trading halts. daily_loss: day P&L strictly below -daily_loss_halt_pct*NAV (strict <, boundary does NOT halt — portfolio spec §3.3); FLAT/close orders still pass during a halt by design.';

CREATE INDEX halts_active_idx ON tms.halts (triggered_at DESC) WHERE cleared_at IS NULL;
CREATE INDEX halts_session_idx ON tms.halts (session_id, triggered_at DESC);

CREATE TRIGGER halts_set_updated_at
    BEFORE UPDATE ON tms.halts
    FOR EACH ROW EXECUTE FUNCTION tms.set_updated_at();

-- ---------------------------------------------------------------------------
-- reconciliation_reports — EOD strategy-books vs broker-net diffs.
-- ---------------------------------------------------------------------------
CREATE TABLE tms.reconciliation_reports (
    id                          BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    session_id                  BIGINT      REFERENCES tms.sessions (id) ON DELETE SET NULL,
    ts                          TIMESTAMPTZ NOT NULL,
    tolerance_shares            INTEGER     NOT NULL DEFAULT 0 CHECK (tolerance_shares >= 0),
    matched                     TEXT[]      NOT NULL DEFAULT '{}',
    mismatches                  JSONB       NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(mismatches) = 'array'),
    symbols_only_in_strategies  TEXT[]      NOT NULL DEFAULT '{}',
    symbols_only_at_broker      TEXT[]      NOT NULL DEFAULT '{}',
    has_issues                  BOOLEAN     GENERATED ALWAYS AS (
                                    jsonb_array_length(mismatches) > 0
                                    OR cardinality(symbols_only_in_strategies) > 0
                                    OR cardinality(symbols_only_at_broker) > 0
                                ) STORED,
    created_at                  TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE tms.reconciliation_reports IS
    'EOD reconciliation (portfolio spec §6): symbols sorted ascending within each list; mismatches = [{symbol, strategy_books_sum, broker_net, diff}] with diff = broker_net - strategy_books_sum (sign matters). has_issues derived exactly as the reference: any of the three issue lists non-empty.';

CREATE INDEX reconciliation_reports_ts_idx ON tms.reconciliation_reports (ts DESC);
CREATE INDEX reconciliation_reports_issues_idx ON tms.reconciliation_reports (ts DESC) WHERE has_issues;
