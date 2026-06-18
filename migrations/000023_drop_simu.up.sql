-- 000023: DELETE the synthetic 'simu' account entirely. Signal sessions now bind
-- the (moomoo, paper) DEFAULT account as a nominal placeholder (NoopExecutor, no
-- orders) instead of a fake simu account; existing simu:signal sessions are
-- repointed to it (NULL when no paper default exists — account_id is nullable).
BEGIN;

-- Repoint any session bound to a synthetic simu account onto the (moomoo, paper)
-- default — or NULL when there is no paper default (the FK column is nullable) — so
-- the session history survives the simu deletion.
UPDATE tms.sessions
   SET account_id = (SELECT id FROM tms.accounts
                      WHERE venue = 'moomoo' AND env = 'paper' AND is_default
                      LIMIT 1)
 WHERE account_id IN (SELECT id FROM tms.accounts WHERE env = 'simu');

-- The simu account's OWN dependent rows are disposable historical data (signal mode
-- produced no orders/fills/positions; the only real rows are reconciliation
-- snapshots). Drop them so the ON DELETE RESTRICT FKs release the account.
DELETE FROM tms.reconciliation_reports WHERE account_id IN (SELECT id FROM tms.accounts WHERE env = 'simu');
DELETE FROM tms.fills                  WHERE account_id IN (SELECT id FROM tms.accounts WHERE env = 'simu');
DELETE FROM tms.orders                 WHERE account_id IN (SELECT id FROM tms.accounts WHERE env = 'simu');
DELETE FROM tms.positions              WHERE account_id IN (SELECT id FROM tms.accounts WHERE env = 'simu');

-- Delete the synthetic simu accounts and tighten the env CHECK to broker-only.
DELETE FROM tms.accounts WHERE env = 'simu';
ALTER TABLE tms.accounts DROP CONSTRAINT accounts_env_check;
ALTER TABLE tms.accounts ADD CONSTRAINT accounts_env_check CHECK (env IN ('paper', 'real'));

COMMIT;
