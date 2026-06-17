package study

// staleness.go implements the read-side staleness override (spec §9.2): when a
// study row reads RUNNING but its heartbeat is stale (> 60s) AND its
// coordinator PID is nil or not alive, the
// reader PRESENTS the status as INTERRUPTED without modifying the stored row.
// This catches a crashed/killed coordinator that left the study permanently
// RUNNING (a zombie study), and provides the liveness signal the spec relies on
// across Docker PID namespaces — where the 20s heartbeat freshness (§6.10), not
// the unreliable PID check, is the de-facto signal (the 60s threshold dominates).

import (
	"os"
	"syscall"
	"time"
)

// staleThreshold is the spec §9.2 / §12 stale-heartbeat threshold (60s).
const staleThreshold = 60 * time.Second

// applyStaleness mutates r.Status to INTERRUPTED in-place when r is a stale
// zombie RUNNING study at instant now (§9.2). Only the presented Status changes;
// every other field (and the DB row) is left intact. Non-RUNNING rows are never
// touched. A missing heartbeat timestamp falls back to updated_at; when both are
// absent the study is treated as NOT stale (we cannot prove staleness).
func applyStaleness(r *StudyRow, now time.Time) {
	if r.Status != StatusRunning {
		return
	}
	ref := r.LastHeartbeatAt
	if ref == nil {
		ref = r.StartedAt
	}
	if ref == nil {
		// No heartbeat and no started_at: fall back to updated_at (always set by
		// the row trigger). The embedded StudyConfig.UpdatedAt carries it.
		u := r.UpdatedAt
		if u.IsZero() {
			return
		}
		ref = &u
	}
	if now.UTC().Sub(ref.UTC()) <= staleThreshold {
		return // heartbeat fresh => healthy
	}
	if pidAlive(r.CoordinatorPID) {
		return // a live coordinator overrides the stale heartbeat
	}
	r.Status = StatusInterrupted
}

// pidAlive reports whether the coordinator PID is present AND alive. A nil PID is
// NOT alive. Liveness uses kill(pid, 0) semantics (signal 0 probes existence
// without delivering a signal). Note PID liveness is unreliable across container
// PID namespaces (§9.2 IMPROVE): a foreign/recycled PID may read "alive", but the
// 60s heartbeat threshold dominates, so a truly dead coordinator is still caught
// once its heartbeat goes stale.
func pidAlive(pid *int) bool {
	if pid == nil || *pid <= 0 {
		return false
	}
	p, err := os.FindProcess(*pid)
	if err != nil {
		return false
	}
	// On Unix FindProcess always succeeds; Signal(0) is the real liveness probe.
	err = p.Signal(syscall.Signal(0))
	if err == nil {
		return true // exists and we can signal it
	}
	if err == syscall.EPERM {
		return true // exists but owned by another user (still alive)
	}
	return false // ESRCH (no such process) or other => dead
}
