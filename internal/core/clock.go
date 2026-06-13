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
// is a SimClock advanced by the loop; a live build would supply a wall clock.
type Clock interface {
	// Now returns the current simulated (or wall) time, always UTC.
	Now() time.Time
}

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
