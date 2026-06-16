-- 000014_accounts: first-class trading ACCOUNTS + per-account attribution.
--
-- Before this, "paper vs live" lived only in tms.sessions.mode and the broker
-- account id was an env var baked into the node at startup — the DB had no
-- account dimension, so positions could not be managed/viewed per account
-- (design: docs/design/trade-refactor.md). This adds:
--   - tms.accounts: the registry (one row per broker/sim account TMS knows).
--   - account_id on sessions/orders/positions/fills/reconciliation_reports.
--
-- account_id is NULLABLE here on purpose: existing rows are backfilled, but the
-- node still creates sessions WITHOUT an account_id until phase 3 wires it — a
-- later migration sets NOT NULL once every writer populates it. signal_intents
-- is deliberately excluded: a signal is account-agnostic (a strategy decision,
-- not an order).

CREATE TABLE tms.accounts (
    -- id is the stable TMS account identity used in attribution + as the FK.
    -- Broker accounts: "<venue>:<env>:<brokerAccID>" (e.g.
    -- "moomoo:real:283445331237495693"). Sim accounts: "sim:<name>".
    id            TEXT        PRIMARY KEY CHECK (id <> ''),
    venue         TEXT        NOT NULL CHECK (venue <> ''),   -- "moomoo" | "sim"
    env           TEXT        NOT NULL CHECK (env IN ('sim', 'simulate', 'real')),
    -- broker_acc_id is the broker's account id; 0 for sim (and for legacy
    -- backfilled broker accounts whose id was not recorded in the DB). The app
    -- (domain.Account.Validate) requires it > 0 for NEW broker accounts.
    broker_acc_id BIGINT      NOT NULL DEFAULT 0 CHECK (broker_acc_id >= 0),
    label         TEXT        NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE tms.accounts IS
    'First-class trading accounts (broker or sim). Orders/positions/fills attribute to one; enables per-account position management.';

ALTER TABLE tms.sessions               ADD COLUMN account_id TEXT REFERENCES tms.accounts (id) ON DELETE RESTRICT;
ALTER TABLE tms.orders                 ADD COLUMN account_id TEXT REFERENCES tms.accounts (id) ON DELETE RESTRICT;
ALTER TABLE tms.positions              ADD COLUMN account_id TEXT REFERENCES tms.accounts (id) ON DELETE RESTRICT;
ALTER TABLE tms.fills                  ADD COLUMN account_id TEXT REFERENCES tms.accounts (id) ON DELETE RESTRICT;
ALTER TABLE tms.reconciliation_reports ADD COLUMN account_id TEXT REFERENCES tms.accounts (id) ON DELETE RESTRICT;

-- Backfill: derive an account per existing session from its mode (+ the broker
-- acc id from config.target_account when present + numeric). signal → a shared
-- sim account; paper/live → a moomoo simulate/real account. Idempotent.
INSERT INTO tms.accounts (id, venue, env, broker_acc_id, label)
SELECT DISTINCT
    CASE s.mode
        WHEN 'signal' THEN 'sim:signal'
        WHEN 'paper'  THEN 'moomoo:simulate:' || CASE WHEN s.config->>'target_account' ~ '^[0-9]+$' THEN s.config->>'target_account' ELSE 'legacy' END
        WHEN 'live'   THEN 'moomoo:real:'     || CASE WHEN s.config->>'target_account' ~ '^[0-9]+$' THEN s.config->>'target_account' ELSE 'legacy' END
    END,
    CASE s.mode WHEN 'signal' THEN 'sim' ELSE 'moomoo' END,
    CASE s.mode WHEN 'signal' THEN 'sim' WHEN 'paper' THEN 'simulate' WHEN 'live' THEN 'real' END,
    CASE WHEN s.config->>'target_account' ~ '^[0-9]+$' THEN (s.config->>'target_account')::BIGINT ELSE 0 END,
    'backfilled from session mode ' || s.mode
FROM tms.sessions s
ON CONFLICT (id) DO NOTHING;

UPDATE tms.sessions s SET account_id =
    CASE s.mode
        WHEN 'signal' THEN 'sim:signal'
        WHEN 'paper'  THEN 'moomoo:simulate:' || CASE WHEN s.config->>'target_account' ~ '^[0-9]+$' THEN s.config->>'target_account' ELSE 'legacy' END
        WHEN 'live'   THEN 'moomoo:real:'     || CASE WHEN s.config->>'target_account' ~ '^[0-9]+$' THEN s.config->>'target_account' ELSE 'legacy' END
    END;

UPDATE tms.orders                 o SET account_id = s.account_id FROM tms.sessions s WHERE o.session_id = s.id;
UPDATE tms.positions              p SET account_id = s.account_id FROM tms.sessions s WHERE p.session_id = s.id;
UPDATE tms.reconciliation_reports r SET account_id = s.account_id FROM tms.sessions s WHERE r.session_id = s.id;
UPDATE tms.fills                  f SET account_id = o.account_id FROM tms.orders   o WHERE f.order_id   = o.id;

-- Per-account position management + blotter indexes.
CREATE INDEX positions_account_strategy_symbol_idx ON tms.positions (account_id, strategy_id, symbol);
CREATE INDEX orders_account_idx                     ON tms.orders    (account_id, created_at DESC);
