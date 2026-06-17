-- Down: restore the legacy tms.sessions.mode {signal,paper,live} and drop
-- exec_policy. mode is reconstructed from exec_policy + the bound account env:
-- signal→signal; auto over a real account→live, otherwise→paper. Sessions with
-- no account_id (or a sim account) fall back to paper for any auto policy.

ALTER TABLE tms.sessions ADD COLUMN mode TEXT
    CHECK (mode IN ('signal', 'paper', 'live'));

UPDATE tms.sessions s SET mode =
    CASE
        WHEN s.exec_policy = 'signal' THEN 'signal'
        WHEN a.env = 'real'           THEN 'live'
        ELSE 'paper'
    END
FROM tms.sessions s2
LEFT JOIN tms.accounts a ON a.id = s2.account_id
WHERE s.id = s2.id;

ALTER TABLE tms.sessions ALTER COLUMN mode SET NOT NULL;

ALTER TABLE tms.sessions DROP COLUMN exec_policy;
