-- Revert 000007_universe_members.

ALTER TABLE tms.universe_snapshots
    DROP COLUMN IF EXISTS members;
