-- 000017_rename_models_to_compositions: pure rename of the "Model" concept to
-- "Composition" (docs/concept-alignment.md). Behaviour is unchanged — this only
-- renames tables, columns, the updated_at trigger, and the auto-named primary
-- key / foreign-key / check constraints introduced in 000015_models so the
-- schema reads "Composition" end-to-end. All ALTER ... RENAME operations
-- preserve data and are exactly reversed in the .down.sql.

-- ---------------------------------------------------------------------------
-- Tables.
-- ---------------------------------------------------------------------------
ALTER TABLE tms.models        RENAME TO compositions;
ALTER TABLE tms.model_members RENAME TO composition_members;

-- ---------------------------------------------------------------------------
-- Columns: the model_id references on composition_members + runtime/history
-- tables, and accounts.default_model_id.
-- ---------------------------------------------------------------------------
ALTER TABLE tms.composition_members RENAME COLUMN model_id         TO composition_id;
ALTER TABLE tms.sessions            RENAME COLUMN model_id         TO composition_id;
ALTER TABLE tms.runs                RENAME COLUMN model_id         TO composition_id;
ALTER TABLE tms.hyperopt_studies    RENAME COLUMN model_id         TO composition_id;
ALTER TABLE tms.accounts            RENAME COLUMN default_model_id TO default_composition_id;

-- ---------------------------------------------------------------------------
-- updated_at trigger.
-- ---------------------------------------------------------------------------
ALTER TRIGGER models_set_updated_at ON tms.compositions
    RENAME TO compositions_set_updated_at;

-- ---------------------------------------------------------------------------
-- Primary-key indexes (RENAME TABLE does not rename the auto-generated PKs).
-- ---------------------------------------------------------------------------
ALTER INDEX tms.models_pkey        RENAME TO compositions_pkey;
ALTER INDEX tms.model_members_pkey RENAME TO composition_members_pkey;

-- ---------------------------------------------------------------------------
-- Check constraints on compositions.
-- ---------------------------------------------------------------------------
ALTER TABLE tms.compositions RENAME CONSTRAINT models_cash_pct_check                 TO compositions_cash_pct_check;
ALTER TABLE tms.compositions RENAME CONSTRAINT models_id_check                       TO compositions_id_check;
ALTER TABLE tms.compositions RENAME CONSTRAINT models_name_check                     TO compositions_name_check;
ALTER TABLE tms.compositions RENAME CONSTRAINT models_risk_concentration_pct_check   TO compositions_risk_concentration_pct_check;
ALTER TABLE tms.compositions RENAME CONSTRAINT models_risk_daily_loss_halt_pct_check TO compositions_risk_daily_loss_halt_pct_check;
ALTER TABLE tms.compositions RENAME CONSTRAINT models_risk_max_gross_pct_check       TO compositions_risk_max_gross_pct_check;
ALTER TABLE tms.compositions RENAME CONSTRAINT models_risk_max_positions_check       TO compositions_risk_max_positions_check;
ALTER TABLE tms.compositions RENAME CONSTRAINT models_risk_single_name_pct_check     TO compositions_risk_single_name_pct_check;
ALTER TABLE tms.compositions RENAME CONSTRAINT models_version_check                  TO compositions_version_check;

-- ---------------------------------------------------------------------------
-- Constraints on composition_members.
-- ---------------------------------------------------------------------------
ALTER TABLE tms.composition_members RENAME CONSTRAINT model_members_model_id_fkey     TO composition_members_composition_id_fkey;
ALTER TABLE tms.composition_members RENAME CONSTRAINT model_members_param_set_id_fkey TO composition_members_param_set_id_fkey;
ALTER TABLE tms.composition_members RENAME CONSTRAINT model_members_strategy_id_check TO composition_members_strategy_id_check;
ALTER TABLE tms.composition_members RENAME CONSTRAINT model_members_weight_check      TO composition_members_weight_check;

-- ---------------------------------------------------------------------------
-- Foreign-key constraints referencing compositions from the other tables.
-- ---------------------------------------------------------------------------
ALTER TABLE tms.accounts         RENAME CONSTRAINT accounts_default_model_id_fkey TO accounts_default_composition_id_fkey;
ALTER TABLE tms.hyperopt_studies RENAME CONSTRAINT hyperopt_studies_model_id_fkey TO hyperopt_studies_composition_id_fkey;
ALTER TABLE tms.runs             RENAME CONSTRAINT runs_model_id_fkey             TO runs_composition_id_fkey;
ALTER TABLE tms.sessions         RENAME CONSTRAINT sessions_model_id_fkey         TO sessions_composition_id_fkey;
