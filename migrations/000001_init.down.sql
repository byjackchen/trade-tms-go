-- Rollback of 000001_init. Dropping the extension is intentionally NOT done
-- here: other databases/objects may depend on timescaledb and the extension
-- is harmless when unused.

DROP SCHEMA IF EXISTS tms CASCADE;
