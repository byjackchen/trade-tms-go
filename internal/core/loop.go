package core

// loop.go is the single-goroutine event loop. It drains the deterministic
// queue in total order, advances the simulated clock, and dispatches each
// event to the registered handler. There is NO concurrency: handlers run
// synchronously on the loop goroutine and may schedule further events (e.g. a
// bar handler submitting orders whose fills are scheduled at the same ts).
//
// Context cancellation is checked between events, so a cancelled run stops
// cleanly at an event boundary (never mid-handler, never leaving partial
// state) and returns ctx.Err().

import (
	"context"
	"fmt"
	"time"
)

// Handler dispatches one event. Returning an error aborts the run (the loop
// surfaces it). Handlers run on the loop goroutine and may call Loop.Schedule
// to enqueue follow-on events at the current timestamp or later.
type Handler interface {
	Handle(ctx context.Context, ev Event) error
}

// HandlerFunc adapts a function to Handler.
type HandlerFunc func(ctx context.Context, ev Event) error

// Handle calls f.
func (f HandlerFunc) Handle(ctx context.Context, ev Event) error { return f(ctx, ev) }

// Loop is the deterministic event loop. Build one with NewLoop, register a
// handler per EventKind, seed initial events with Schedule, then Run.
//
// Not safe for concurrent use: Schedule is intended to be called from the loop
// goroutine (i.e. from within a handler) or before Run. Seeding from another
// goroutine while Run executes is unsupported.
type Loop struct {
	clock    *SimClock
	queue    *eventQueue
	handlers map[EventKind]Handler
	closed   bool
}

// NewLoop returns a loop with its own SimClock and empty queue.
func NewLoop() *Loop {
	return &Loop{
		clock:    NewSimClock(),
		queue:    newEventQueue(),
		handlers: make(map[EventKind]Handler),
	}
}

// Clock returns the loop's simulated clock (read-only use by handlers).
func (l *Loop) Clock() Clock { return l.clock }

// Register installs the handler for a kind, replacing any previous one. Call
// before Run.
func (l *Loop) Register(kind EventKind, h Handler) { l.handlers[kind] = h }

// Schedule enqueues ev, returning its assigned deterministic sequence. It
// rejects events timestamped before the current clock (ErrTimeReversal) and
// events scheduled after the loop has closed (ErrLoopClosed). Events at the
// current clock time are allowed (same-timestamp follow-ons, e.g. fills).
func (l *Loop) Schedule(ev Event) (uint64, error) {
	if l.closed {
		return 0, ErrLoopClosed
	}
	ts := ev.TsEvent()
	if _, off := ts.Zone(); off != 0 {
		return 0, fmt.Errorf("%w: scheduling non-UTC event %s at %s", ErrTimeReversal, ev.Kind(), ts)
	}
	if now := l.clock.Now(); !now.IsZero() && ts.Before(now) {
		return 0, fmt.Errorf("%w: scheduling %s at %s before clock %s", ErrTimeReversal, ev.Kind(), ts, now)
	}
	return l.queue.push(ev), nil
}

// NextSeq returns a fresh deterministic sequence value from the queue's
// counter, for callers that need a unique monotonic token (e.g. client order
// ids) consistent with event sequencing.
func (l *Loop) NextSeq() uint64 { return l.queue.nextSeq() }

// Now returns the current simulated time.
func (l *Loop) Now() time.Time { return l.clock.Now() }

// Run drains the queue until empty or ctx is cancelled. It advances the clock
// to each event's timestamp before dispatch and routes the event to its
// registered handler. A missing handler for a popped kind is a programming
// error and aborts the run. On return the loop is closed (no further
// Schedule). Returns nil on clean drain, ctx.Err() on cancellation, or the
// first handler/clock error.
func (l *Loop) Run(ctx context.Context) (err error) {
	defer func() { l.closed = true }()
	for {
		// Check cancellation at each event boundary for a clean stop.
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		ev, ok := l.queue.pop()
		if !ok {
			return nil // drained cleanly
		}
		if aerr := l.clock.Advance(ev.TsEvent()); aerr != nil {
			return fmt.Errorf("loop advancing clock for %s: %w", ev.Kind(), aerr)
		}
		h, ok := l.handlers[ev.Kind()]
		if !ok {
			return fmt.Errorf("loop: no handler registered for event kind %s", ev.Kind())
		}
		if herr := h.Handle(ctx, ev); herr != nil {
			return fmt.Errorf("loop handling %s at %s: %w", ev.Kind(), ev.TsEvent().Format(time.RFC3339Nano), herr)
		}
	}
}
