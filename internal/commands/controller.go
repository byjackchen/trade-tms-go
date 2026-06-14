package commands

// controller.go defines the live-node control surface the consumer drives, plus
// a concrete HaltState the live node embeds. The kill-switch / halt mechanism
// (decision 6) is: stop emitting NEW intents + set a halt flag. In signal mode
// there are no positions, so FLATTEN is a no-op (deferred to P6). The live
// session checks Emitting() before publishing each timestamp's intents.

import (
	"context"
	"sync"
	"time"
)

// HaltKind classifies a halt (mirrors tms.halts.kind).
type HaltKind string

const (
	HaltManual         HaltKind = "manual"
	HaltDailyLoss      HaltKind = "daily_loss"
	HaltReconciliation HaltKind = "reconciliation"
	HaltData           HaltKind = "data"
	HaltBroker         HaltKind = "broker"
	HaltOther          HaltKind = "other"
)

// HaltState is the live node's thread-safe halt + emit gate. The command
// consumer mutates it; the live session reads Emitting() before each emit.
// Zero value is "running, not halted, emitting".
type HaltState struct {
	mu         sync.RWMutex
	halted     bool
	haltKind   HaltKind
	haltReason string
	haltedAt   time.Time
	stopped    bool // a stop/kill was requested (the node should exit)
	now        func() time.Time
}

// NewHaltState returns a fresh running state. now defaults to time.Now.
func NewHaltState(now func() time.Time) *HaltState {
	if now == nil {
		now = time.Now
	}
	return &HaltState{now: now}
}

// Halt sets the halt flag with a kind + reason. Idempotent: re-halting keeps the
// FIRST halt's timestamp (the original trigger), updating kind/reason only if
// not already halted.
func (h *HaltState) Halt(kind HaltKind, reason string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.halted {
		h.halted = true
		h.haltKind = kind
		h.haltReason = reason
		h.haltedAt = h.now().UTC()
	}
}

// HaltDailyLoss latches a daily-loss halt (HaltDailyLoss kind) with reason. It
// is the livetrade.Halter entry point the pre-submit gate calls when day P&L
// crosses -daily_loss_halt_pct*NAV. Idempotent: a re-halt keeps the first
// trigger's timestamp/kind (so a manual halt already in effect is not
// downgraded). After this latches, the gate rejects NEW opening orders; existing
// positions stay open and FLAT/close orders still pass (portfolio-risk.md §3.3).
func (h *HaltState) HaltDailyLoss(reason string) {
	h.Halt(HaltDailyLoss, reason)
}

// Resume clears the halt flag (does NOT clear a stop request).
func (h *HaltState) Resume() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.halted = false
	h.haltKind = ""
	h.haltReason = ""
	h.haltedAt = time.Time{}
}

// Stop requests a graceful node exit (set by stop/kill). Irreversible within a
// run.
func (h *HaltState) Stop() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stopped = true
}

// Emitting reports whether the node may emit NEW intents: true iff not halted
// and not stopped. The live session gates each timestamp's emission on this.
func (h *HaltState) Emitting() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return !h.halted && !h.stopped
}

// IsHalted reports the halt flag.
func (h *HaltState) IsHalted() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.halted
}

// IsStopped reports whether a stop/kill was requested.
func (h *HaltState) IsStopped() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.stopped
}

// Snapshot returns a consistent read of the halt state (for /api/v1/live/session
// and audit).
func (h *HaltState) Snapshot() HaltSnapshot {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return HaltSnapshot{
		Halted:   h.halted,
		Kind:     h.haltKind,
		Reason:   h.haltReason,
		HaltedAt: h.haltedAt,
		Stopped:  h.stopped,
	}
}

// HaltSnapshot is an immutable read of HaltState.
type HaltSnapshot struct {
	Halted   bool      `json:"halted"`
	Kind     HaltKind  `json:"kind,omitempty"`
	Reason   string    `json:"reason,omitempty"`
	HaltedAt time.Time `json:"halted_at,omitempty"`
	Stopped  bool      `json:"stopped"`
}

// Controller is the live-node control surface the command consumer drives. The
// live runner supplies an implementation backed by the running session +
// HaltState. Every method is idempotent (a re-delivered command is a no-op when
// the node is already in the requested state).
type Controller interface {
	// Mode returns the current execution mode (signal|paper|live).
	Mode() string
	// SetMode requests a mode switch. In P5 only "signal" is accepted; paper/live
	// return an error (deferred to P6). A graceful session restart is the live
	// runner's responsibility — SetMode records the intent and the runner acts.
	SetMode(ctx context.Context, mode string) error
	// Halt stops emitting new intents and sets the halt state.
	Halt(ctx context.Context, kind HaltKind, reason string) error
	// Resume clears a manual halt and resumes emitting.
	Resume(ctx context.Context) error
	// Stop requests a graceful node shutdown.
	Stop(ctx context.Context, reason string) error
	// Kill is the kill switch: halt + stop (hard).
	Kill(ctx context.Context, reason string) error
	// Flatten closes ALL open positions with FLAT market orders (P6 decision 7).
	// Paper/live only (signal mode has no positions: a no-op/err). Idempotent +
	// confirmation-gated upstream. Returns the count of closing orders submitted.
	Flatten(ctx context.Context, reason string) (int, error)
	// EmergencyKill is the panic button (P6 decision 5): halt + flatten + stop.
	// It halts first (suppress new opens), flattens (close everything), then stops
	// the node. Returns the count of closing orders submitted.
	EmergencyKill(ctx context.Context, reason string) (int, error)
	// Reconcile runs an on-demand reconciliation (P6 decision 5): compare broker
	// positions vs strategy books, persist a report, alert on a mismatch. Returns
	// whether the report had issues (drift). NO auto-correct.
	Reconcile(ctx context.Context) (hasIssues bool, err error)
}
