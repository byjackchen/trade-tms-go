-- 000017_rename_models_to_compositions (down): reverse every rename, restoring
-- the "Model" identifiers from 000015_models exactly.

-- ---------------------------------------------------------------------------
-- Foreign-key constraints referencing compositions from the other tables.
-- ---------------------------------------------------------------------------
ALTER TABLE tms.sessions         RENAME CONSTRAINT sessions_composition_id_fkey         TO sessions_model_id_fkey;
ALTER TABLE tms.runs             RENAME CONSTRAINT runs_composition_id_fkey             TO runs_model_id_fkey;
ALTER TABLE tms.hyperopt_studies RENAME CONSTRAINT hyperopt_studies_composition_id_fkey TO hyperopt_studies_model_id_fkey;
ALTER TABLE tms.accounts         RENAME CONSTRAINT accounts_default_composition_id_fkey TO accounts_default_model_id_fkey;

-- ---------------------------------------------------------------------------
-- Constraints on composition_members.
-- ---------------------------------------------------------------------------
ALTER TABLE tms.composition_members RENAME CONSTRAINT composition_members_weight_check          TO model_members_weight_check;
ALTER TABLE tms.composition_members RENAME CONSTRAINT composition_members_strategy_id_check     TO model_members_strategy_id_check;
ALTER TABLE tms.composition_members RENAME CONSTRAINT composition_members_param_set_id_fkey     TO model_members_param_set_id_fkey;
ALTER TABLE tms.composition_members RENAME CONSTRAINT composition_members_composition_id_fkey   TO model_members_model_id_fkey;

-- ---------------------------------------------------------------------------
-- Check constraints on compositions.
-- ---------------------------------------------------------------------------
ALTER TABLE tms.compositions RENAME CONSTRAINT compositions_version_check                  TO models_version_check;
ALTER TABLE tms.compositions RENAME CONSTRAINT compositions_risk_single_name_pct_check     TO models_risk_single_name_pct_check;
ALTER TABLE tms.compositions RENAME CONSTRAINT compositions_risk_max_positions_check       TO models_risk_max_positions_check;
ALTER TABLE tms.compositions RENAME CONSTRAINT compositions_risk_max_gross_pct_check       TO models_risk_max_gross_pct_check;
ALTER TABLE tms.compositions RENAME CONSTRAINT compositions_risk_daily_loss_halt_pct_check TO models_risk_daily_loss_halt_pct_check;
ALTER TABLE tms.compositions RENAME CONSTRAINT compositions_risk_concentration_pct_check   TO models_risk_concentration_pct_check;
ALTER TABLE tms.compositions RENAME CONSTRAINT compositions_name_check                     TO models_name_check;
ALTER TABLE tms.compositions RENAME CONSTRAINT compositions_id_check                       TO models_id_check;
ALTER TABLE tms.compositions RENAME CONSTRAINT compositions_cash_pct_check                 TO models_cash_pct_check;

-- ---------------------------------------------------------------------------
-- Primary-key indexes.
-- ---------------------------------------------------------------------------
ALTER INDEX tms.composition_members_pkey RENAME TO model_members_pkey;
ALTER INDEX tms.compositions_pkey        RENAME TO models_pkey;

-- ---------------------------------------------------------------------------
-- updated_at trigger.
-- ---------------------------------------------------------------------------
ALTER TRIGGER compositions_set_updated_at ON tms.compositions
    RENAME TO models_set_updated_at;

-- ---------------------------------------------------------------------------
-- Columns.
-- ---------------------------------------------------------------------------
ALTER TABLE tms.accounts            RENAME COLUMN default_composition_id TO default_model_id;
ALTER TABLE tms.hyperopt_studies    RENAME COLUMN composition_id        TO model_id;
ALTER TABLE tms.runs                RENAME COLUMN composition_id        TO model_id;
ALTER TABLE tms.sessions            RENAME COLUMN composition_id        TO model_id;
ALTER TABLE tms.composition_members RENAME COLUMN composition_id        TO model_id;

-- ---------------------------------------------------------------------------
-- Tables.
-- ---------------------------------------------------------------------------
ALTER TABLE tms.composition_members RENAME TO model_members;
ALTER TABLE tms.compositions        RENAME TO models;
