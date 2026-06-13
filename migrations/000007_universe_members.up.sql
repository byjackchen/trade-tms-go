-- 000007_universe_members: ranked member detail for universe snapshots.
--
-- P1 (internal/data/universe): each snapshot now records, next to the plain
-- ordered ticker list, a JSONB array of ranked members with the full SEPA
-- screener diagnostics (rank, score, trend-template count, breakout
-- proximity, market cap) and human-readable reasons (the passing
-- trend-template rule names). This makes every persisted universe
-- self-explanatory for audit/UI without re-running the screener.

ALTER TABLE tms.universe_snapshots
    ADD COLUMN members JSONB NOT NULL DEFAULT '[]'::jsonb
        CHECK (jsonb_typeof(members) = 'array');

COMMENT ON COLUMN tms.universe_snapshots.members IS
    'Ranked member array: [{ticker, rank (1-based, ranking order), score, trend_template_count, breakout_proximity, market_cap_usd, reasons[]}]. reasons = passing Minervini trend-template rule names (docs/spec/calendar-universe.md §3.5). Non-finite floats are stored as 0 (JSON cannot carry NaN/Inf).';
