-- 000001_init: bootstrap migration.
-- Establishes the tms schema and the TimescaleDB extension so every later
-- migration can assume both exist. Business tables arrive in later
-- migrations (P0 data import phase).

CREATE EXTENSION IF NOT EXISTS timescaledb;

CREATE SCHEMA IF NOT EXISTS tms;

COMMENT ON SCHEMA tms IS 'Trade Management System — all application objects live here.';
