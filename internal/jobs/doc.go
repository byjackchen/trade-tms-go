// Package jobs is the durable job-orchestration backbone: a PostgreSQL
// queue (tms.jobs, migrations 000006 + 000008) with FOR UPDATE SKIP LOCKED
// claiming, heartbeat-based stale-claim recovery, cooperative cancellation,
// retry-with-backoff, JSONB progress reporting and a full audit trail.
//
// # Lifecycle
//
//	queued ──claim──▶ running ──▶ succeeded
//	  ▲                  │    └──▶ failed     (attempts exhausted)
//	  │                  ├───────▶ canceled   (cooperative cancel honored)
//	  └──────────────────┘        requeue paths:
//	                              · Fail with attempts < max_attempts (backoff)
//	                              · Release on worker drain (attempt refunded)
//	                              · ReapStale after heartbeat TTL expiry
//
// Status spelling is "canceled" (single l) — fixed by the 000006 CHECK
// constraint; this package mirrors the database spelling.
//
// # Claiming
//
// Claim executes the canonical statement documented in the 000006 DDL
// header: a single UPDATE whose subquery selects the next eligible queued
// row with FOR UPDATE SKIP LOCKED, so any number of workers can poll
// concurrently without lock contention or double delivery.
//
// # Heartbeats and stale-claim recovery
//
// The claiming worker bumps heartbeat_at every HeartbeatInterval; the same
// round-trip returns cancel_requested, making the heartbeat double as the
// cooperative-cancel signal. ReapStale (run periodically by every worker;
// SKIP LOCKED makes it safe under concurrency) re-queues running jobs whose
// heartbeat is older than the TTL — the worker died — or marks them
// failed/canceled when attempts are exhausted / cancel was requested.
//
// # Cancellation
//
// Cancel on a queued job finishes it immediately. Cancel on a running job
// sets cancel_requested; the owning worker observes the flag on its next
// heartbeat (or progress report), cancels the handler's context, and writes
// the terminal canceled state. Handlers therefore only need to honor
// context cancellation to be cancelable.
//
// # Observability
//
// Every state change appends a tms.audit_log row (actions job.enqueued,
// job.claimed, job.succeeded, job.failed, job.requeued, job.released,
// job.canceled, job.cancel_requested, job.reaped) and publishes a JSON
// Event to the Redis pub/sub channel DefaultEventsChannel ("tms:jobs:events")
// for live UI updates. Redis publishing is strictly best-effort: failures
// are logged and never affect queue state. Progress reports update the
// jobs.progress JSONB column and publish an event, but are deliberately
// not audited (too chatty for an append-only audit table).
//
// The jobs event channel is a Go-side addition (no Python reference
// equivalent — the reference has no durable job queue); documented here as
// the single source of truth for its wire shape (see Event).
package jobs
