-- Reverses 000023: re-allow the 'simu' env. The deleted synthetic simu accounts
-- are NOT restored (they are regenerable placeholders).
BEGIN;
ALTER TABLE tms.accounts DROP CONSTRAINT accounts_env_check;
ALTER TABLE tms.accounts ADD CONSTRAINT accounts_env_check CHECK (env IN ('simu', 'paper', 'real'));
COMMIT;
