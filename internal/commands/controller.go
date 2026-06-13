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
}
