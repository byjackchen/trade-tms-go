// Package publish is the live-telemetry egress layer: it normalizes the
// heterogeneous per-strategy intent values returned by engine.SignalEvaluator
// into the single canonical snake_case wire shape the cockpit decodes
// (api-ws-redis.md §2.6/§5.9), derives the tms.signals discriminator
// columns, persists rows to Postgres (the durable truth) and fans the same
// payloads out over Redis streams (the hot mirror the UI follows). It is the
// ONLY place that knows each strategy's concrete intent shape; downstream
// transport (Redis) is best-effort — Postgres is truth, a publish failure is
// the caller's to swallow (P6 decision 5).
//
// Layer: outbound adapter / egress (sits below runner, above domain). Transport
// and serialization only — it makes no trading decisions and runs no strategy
// or engine logic.
//
// May import: internal/domain (the canonical wire types it marshals) plus the
// standard library and the Redis/pgx clients. It switches ONLY on the canonical
// domain intent types — the per-strategy local→domain normalization lives in the
// sanctioned adapter bridges (sepaadapter/orbadapter; modularization-review.md
// §E3), so publish imports NO concrete strategy package. It must NOT import
// internal/engine, internal/runner or any adapter package.
package publish
