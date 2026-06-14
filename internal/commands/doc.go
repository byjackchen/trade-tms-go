// Package commands is the live control plane: the ops.commands consumer that
// the tms-live node runs (P5 locked decision 6). Operators / the API enqueue
// commands into tms.commands (target = "live") with a Redis notify; this
// consumer claims them idempotently, applies them to the running node via a
// Controller, and writes a full tms.audit_log trail.
//
// Command set (decision 6):
//
//	start          — (re)start emitting intents (clears a manual halt)
//	stop           — stop the node gracefully (drain + exit)
//	set_mode       — switch signal|paper|live (paper/live deferred to P6: the
//	                 consumer records the request + audits, but the node rejects
//	                 the switch until P6 wires order submission)
//	halt           — stop emitting NEW intents + set halt state (FLATTEN deferred
//	                 to P6; in signal mode there are no positions to flatten)
//	resume         — clear a manual halt, resume emitting intents
//	kill           — kill switch: halt + stop the node (hard stop)
//
// The trading mutation surface stays OUT of the HTTP API (api spec §1.1 —
// read-only forever); commands are the audited side channel. The API only
// ENQUEUES (POST /api/v1/live/commands); this consumer is the ONLY executor.
//
// Layer: control-plane infrastructure. It defines the Controller seam the live
// node (internal/runner) implements; the node depends on commands, not the
// other way around. It has no strategy or engine logic.
//
// May import: the standard library and the persistence/notify clients (pgx,
// go-redis, zerolog) only. It defines the Controller/HaltState interfaces
// locally and must NOT import internal/engine, internal/livengine,
// internal/runner or any strategy package.
package commands
