package core

// clock.go defines the Clock seam and a deterministic simulated clock driven
// by event timestamps. The Python reference relies on Nautilus's TestClock,
// which advances to each data point's ts before dispatch; SimClock is the Go
// analog.

import (
	"fmt"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// Clock is the time source the engine and its handlers read. In a backtest it
// is a SimClock advanced by the loop; a live build supplies a WallClock (real
// time) or a VirtualClock (a controllable wall clock for deterministic tests).
type Clock interface {
	// Now returns the current simulated (or wall) time, always UTC.
	Now() time.Time
}

// WallClock is the real-time clock for the live engine: Now reads the host
// wall clock (UTC). It is the live counterpart of SimClock — instead of being
// advanced to each event's timestamp by the loop, it simply reports real time.
//
// WallClock is safe for concurrent use (time.Now is); the live loop reads it
// from the single dispatch goroutine while a heartbeat/health goroutine may
// read it too.
type WallClock struct{}

// NewWallClock returns a real-time clock.
func NewWallClock() WallClock { return WallClock{} }

// Now returns the current host wall time in UTC.
func (WallClock) Now() time.Time { return time.Now().UTC() }

// VirtualClock is a controllable wall clock: it reports whatever instant it was
// last Set to, never reading the host clock. It is the deterministic-test
// counterpart of WallClock — a live-engine test drives it forward in lockstep
// with the bars it injects, so a streaming run over a virtual clock is
// reproducible bit-for-bit (the consistency-proof anchor).
//
// Unlike SimClock, VirtualClock does NOT enforce monotonicity on Set (a test
// may rewind it); the streaming loop that drives it advances it monotonically.
// It is NOT safe for concurrent use; drive it from one goroutine.
type VirtualClock struct {
	now time.Time
}

// NewVirtualClock returns a clock positioned at t (UTC). A zero t leaves the
// clock at the zero time until the first Set.
func NewVirtualClock(t time.Time) *VirtualClock {
	return &VirtualClock{now: t.UTC()}
}

// Now returns the clock's current instant in UTC.
func (c *VirtualClock) Now() time.Time { return c.now }

// Set moves the clock to t (stored as UTC). No monotonicity check: tests own
// the sequence. The streaming loop only ever moves it forward.
func (c *VirtualClock) Set(t time.Time) { c.now = t.UTC() }

// SimClock is a monotonic, event-driven clock. It starts at the zero time and
// is advanced by the loop to each dispatched event's timestamp. Advancing is
// monotonic non-decreasing: an attempt to move backwards is a programming
// error and returns ErrTimeReversal.
//
// SimClock is NOT safe for concurrent use; the engine drives it from the
// single loop goroutine only.
type SimClock struct {
	now time.Time
}

// NewSimClock returns a clock positioned at the zero time (before any event).
func NewSimClock() *SimClock { return &SimClock{} }

// Now returns the current simulated time in UTC.
func (c *SimClock) Now() time.Time { return c.now }

// Advance moves the clock to t, which must be UTC and not before the current
// time. Advancing to the same instant is allowed (multiple events share a
// timestamp). Returns ErrNotUTC or ErrTimeReversal on violation.
func (c *SimClock) Advance(t time.Time) error {
	if _, off := t.Zone(); off != 0 {
		return fmt.Errorf("%w: advancing SimClock to non-UTC %s", domain.ErrInvalidArgument, t)
	}
	if !c.now.IsZero() && t.Before(c.now) {
		return fmt.Errorf("%w: advancing SimClock from %s back to %s", ErrTimeReversal, c.now, t)
	}
	c.now = t
	return nil
}
