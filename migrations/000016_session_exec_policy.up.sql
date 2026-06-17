-- 000016_session_exec_policy: replace the legacy tms.sessions.mode {signal,paper,
-- live} with the 2D model — exec_policy {signal,auto} × the account env carried by
-- account_id (tms.accounts.env, added in 000014). The conflated three-valued Mode
-- enum is retired here (decision D2b, docs/concept-alignment.md §1.3/§4):
--
--   - exec_policy: what happens to a signal (signal = emit-only, auto = auto-submit).
--   - account env (sim/simulate/real): WHERE orders settle, via account_id.
--
-- "paper vs live" was never an execution property — it is the bound account's env.
-- Backfill maps signal→signal and paper/live→auto; the paper/live distinction is
-- already preserved in account_id (sim:signal / moomoo:simulate:* / moomoo:real:*).

-- Add exec_policy; backfill from the legacy mode, then enforce NOT NULL.
ALTER TABLE tms.sessions ADD COLUMN exec_policy TEXT
    CHECK (exec_policy IN ('signal', 'auto'));

UPDATE tms.sessions SET exec_policy =
    CASE mode
        WHEN 'signal' THEN 'signal'
        WHEN 'paper'  THEN 'auto'
        WHEN 'live'   THEN 'auto'
    END;

ALTER TABLE tms.sessions ALTER COLUMN exec_policy SET NOT NULL;

-- Drop the legacy mode column (and its CHECK falls away with it).
ALTER TABLE tms.sessions DROP COLUMN mode;

COMMENT ON COLUMN tms.sessions.exec_policy IS
    'Execution policy: signal = emit-only (no orders), auto = auto-submit against the bound account. The paper-vs-live axis lives in account_id (tms.accounts.env).';
