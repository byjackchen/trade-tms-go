-- 000021_drop_manual_book (down): no-op. The deleted hand-typed 'MANUAL' book
-- rows (positions/orders/risk_events) are not recoverable from the schema alone,
-- and the manual ORDER ENTRY path that produced them no longer exists, so there
-- is nothing to restore. Rolling back is intentionally a no-op.
SELECT 1;
