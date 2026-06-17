-- 000018_hyperopt_study_kind: distinguish single-strategy hyperopt from the new
-- composition (joint-over-a-Composition) hyperopt on tms.hyperopt_studies.
--
-- A study is now one of two KINDS (docs/concept-alignment.md §1.2, the "Optimize
-- this Composition" path):
--
--   - 'strategy'    — the legacy single/joint single-strategy tune (strategy +
--                     SearchSpaceStrategies space). This is the DEFAULT, so every
--                     existing row keeps its current meaning untouched.
--   - 'composition' — tune the member weights / cash / composite risk of a
--                     Composition (composition_id, added in 000015/000017).
--
-- search_config persists the per-launch search RANGES a composition study ran
-- with (member raw-weight / raw-cash / risk dims, decision 2: GLOBAL defaults but
-- overridable in the launch body). It is NULL for strategy studies, whose space
-- is fixed by the embedded baseline JSON and needs no per-study config.

ALTER TABLE tms.hyperopt_studies
    ADD COLUMN kind TEXT NOT NULL DEFAULT 'strategy'
        CHECK (kind IN ('strategy', 'composition'));

ALTER TABLE tms.hyperopt_studies
    ADD COLUMN search_config JSONB
        CHECK (search_config IS NULL OR jsonb_typeof(search_config) = 'object');

-- Relax the strategy CHECK to admit the composition-study marker token. A
-- composition study carries no single strategy (the strategies come from the
-- target's active members); it stores strategy='composition' so the study name /
-- row stays well-formed. The legacy strategy values keep their meaning.
ALTER TABLE tms.hyperopt_studies
    DROP CONSTRAINT IF EXISTS hyperopt_studies_strategy_check;
ALTER TABLE tms.hyperopt_studies
    ADD CONSTRAINT hyperopt_studies_strategy_check
        CHECK (strategy IN ('sepa', 'sector_rotation', 'pairs', 'joint', 'composition'));

-- A composition study must carry a composition_id; a strategy study must not.
ALTER TABLE tms.hyperopt_studies
    ADD CONSTRAINT hyperopt_studies_kind_composition_check
        CHECK (
            (kind = 'composition' AND composition_id IS NOT NULL)
            OR (kind = 'strategy' AND composition_id IS NULL)
        );

COMMENT ON COLUMN tms.hyperopt_studies.kind IS
    'Study kind: strategy = single/joint single-strategy tune (legacy, default); composition = tune a Composition''s weights/cash/risk (composition_id). docs/concept-alignment.md §1.2.';
COMMENT ON COLUMN tms.hyperopt_studies.search_config IS
    'Composition studies only: the per-launch search ranges (member raw-weight / raw-cash / risk dims). NULL for strategy studies (space fixed by the baseline JSON).';
