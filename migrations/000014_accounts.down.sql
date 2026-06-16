-- Reverse 000014_accounts: drop the per-account attribution + the registry.

DROP INDEX IF EXISTS tms.orders_account_idx;
DROP INDEX IF EXISTS tms.positions_account_strategy_symbol_idx;

ALTER TABLE tms.reconciliation_reports DROP COLUMN IF EXISTS account_id;
ALTER TABLE tms.fills                  DROP COLUMN IF EXISTS account_id;
ALTER TABLE tms.positions              DROP COLUMN IF EXISTS account_id;
ALTER TABLE tms.orders                 DROP COLUMN IF EXISTS account_id;
ALTER TABLE tms.sessions               DROP COLUMN IF EXISTS account_id;

DROP TABLE IF EXISTS tms.accounts;
