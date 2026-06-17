-- Reverse 000015_models: drop the model_id references, then the two tables.

ALTER TABLE tms.accounts         DROP COLUMN IF EXISTS default_model_id;
ALTER TABLE tms.hyperopt_studies DROP COLUMN IF EXISTS model_id;
ALTER TABLE tms.runs             DROP COLUMN IF EXISTS model_id;
ALTER TABLE tms.sessions         DROP COLUMN IF EXISTS model_id;

DROP TABLE IF EXISTS tms.model_members;
DROP TABLE IF EXISTS tms.models;
