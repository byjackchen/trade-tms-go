-- 000018_hyperopt_study_kind (down): drop the kind + search_config columns and
-- the kind/composition cross-check, and restore 000017's strategy CHECK (no
-- 'composition' marker), returning tms.hyperopt_studies to its 000017 shape.

ALTER TABLE tms.hyperopt_studies
    DROP CONSTRAINT IF EXISTS hyperopt_studies_kind_composition_check;

ALTER TABLE tms.hyperopt_studies
    DROP CONSTRAINT IF EXISTS hyperopt_studies_strategy_check;
ALTER TABLE tms.hyperopt_studies
    ADD CONSTRAINT hyperopt_studies_strategy_check
        CHECK (strategy IN ('sepa', 'sector_rotation', 'pairs', 'joint'));

ALTER TABLE tms.hyperopt_studies DROP COLUMN search_config;
ALTER TABLE tms.hyperopt_studies DROP COLUMN kind;
