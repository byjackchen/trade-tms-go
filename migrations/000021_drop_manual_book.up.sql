-- 000021_drop_manual_book: purge the stale hand-typed "MANUAL" book rows.
--
-- The operator-driven manual ORDER ENTRY path (place/cancel/close a hand-typed
-- order via TMS) is being removed: orders are now placed at the broker directly,
-- and TMS only PULLS externally-placed positions back in via a read-only broker
-- SYNC under a fresh "EXTERNAL" book (ManualStrategyID renamed MANUAL -> EXTERNAL).
--
-- The cleanup orphans every row keyed by the old 'MANUAL' strategy_id in the
-- mutable runtime tables. Delete them so the new EXTERNAL synced book starts
-- clean; the EXTERNAL book is repopulated from the broker, not migrated from
-- these stale manual-entry rows.
--
-- Scope is deliberately narrow: only tms.positions, tms.orders and
-- tms.risk_events. tms.audit_log is historical and is NOT touched, and no schema
-- (columns/constraints) is altered — this is pure data cleanup.
-- Idempotent: re-running deletes nothing once the rows are gone.

DELETE FROM tms.positions   WHERE strategy_id = 'MANUAL';
DELETE FROM tms.orders      WHERE strategy_id = 'MANUAL';
DELETE FROM tms.risk_events WHERE strategy_id = 'MANUAL';
