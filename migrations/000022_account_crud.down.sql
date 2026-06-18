-- Reverses 000022_account_crud: drop the management columns + index and rename the
-- BrokerEnv values back ('simu' -> 'sim', 'paper' -> 'simulate').
BEGIN;

DROP INDEX IF EXISTS tms.accounts_one_default_per_env;
ALTER TABLE tms.accounts DROP COLUMN IF EXISTS notes;
ALTER TABLE tms.accounts DROP COLUMN IF EXISTS is_default;

ALTER TABLE tms.accounts DROP CONSTRAINT accounts_env_check;
UPDATE tms.accounts SET env = 'sim'      WHERE env = 'simu';
UPDATE tms.accounts SET env = 'simulate' WHERE env = 'paper';
ALTER TABLE tms.accounts
    ADD CONSTRAINT accounts_env_check CHECK (env IN ('sim', 'simulate', 'real'));

COMMIT;
