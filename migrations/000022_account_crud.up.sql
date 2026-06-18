-- 000022_account_crud: accounts become first-class, USER-MANAGED entities (CRUD
-- from the UI; no more .env-derived account ids) + the BrokerEnv value rename.
--
--   * env rename: 'sim' (synthetic, no broker) -> 'simu'; 'simulate' (broker PAPER
--     account) -> 'paper'; 'real' (broker REAL money) unchanged. The data is
--     migrated BEFORE the value CHECK is swapped.
--   * CRUD fields: is_default (THE account a `tms trade run --env paper|real` binds
--     when no explicit account is given), notes, updated_at.
--   * at most ONE default per (venue, env).
--
-- The account id stays an OPAQUE surrogate: legacy rows keep their original
-- "<venue>:<env>:<brokerAccID>" id (now decoupled from the env column, which may
-- have been renamed); new UI-created accounts get an "acct_<uuid>" id. Editing
-- venue/env/broker_acc_id never rewrites the id, so FK history stays intact.
BEGIN;

-- BrokerEnv value rename (data first, then the CHECK).
ALTER TABLE tms.accounts DROP CONSTRAINT accounts_env_check;
UPDATE tms.accounts SET env = 'simu'  WHERE env = 'sim';
UPDATE tms.accounts SET env = 'paper' WHERE env = 'simulate';
ALTER TABLE tms.accounts
    ADD CONSTRAINT accounts_env_check CHECK (env IN ('simu', 'paper', 'real'));

-- Management columns (updated_at already exists from 000014).
ALTER TABLE tms.accounts ADD COLUMN is_default BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE tms.accounts ADD COLUMN notes      TEXT    NOT NULL DEFAULT '';

-- Preserve existing single-account-per-env setups: now that `tms trade run --env`
-- binds the (venue, env) DEFAULT (no more .env acc ids), mark one account per group
-- as the default so existing runs keep resolving without a manual UI step. DISTINCT
-- ON picks the earliest-created account in each (venue, env).
UPDATE tms.accounts a SET is_default = true
  FROM (SELECT DISTINCT ON (venue, env) id
          FROM tms.accounts
         ORDER BY venue, env, created_at, id) d
 WHERE a.id = d.id;

-- At most one default per (venue, env): the partial unique index lets every env
-- have exactly one default while leaving non-defaults unconstrained.
CREATE UNIQUE INDEX accounts_one_default_per_env
    ON tms.accounts (venue, env) WHERE is_default;

COMMIT;
