package core

// stream.go is the ONLINE (streaming) variant of the event loop for the live
// engine. The batch loop (loop.go) drains a pre-seeded queue; the streaming
// loop instead receives events one at a time as a feed delivers them over
// time, advances a caller-supplied Clock, and dispatches each through the same
// registered handlers.
//
// # Two clock disciplines, one dispatch path
//
//   - Live (WallClock): the clock follows REAL time. Events arrive as the
//     market produces bars; the loop dispatches each on arrival. The clock is
//     read, never advanced by the loop (time.Now drives it). Event timestamps
//     are still required to be UTC and non-decreasing (a feed that delivers a
//     bar older than the last is a programming/data error).
//
//   - Virtual (VirtualClock): the clock follows EVENT time, exactly like
//     SimClock — the loop Sets it to each event's TsEvent before dispatch. This
//     makes a streaming run fully deterministic: the same bars delivered in the
//     same order always produce the same dispatches, regardless of how fast the
//     test feeds them. This is the consistency-proof discipline (live path ==
//     batch path).
//
// The streaming loop reuses the Loop's handler registry and SimClock-free
// determinism guarantees, but owns its own monotonic-timestamp gate (the
// batch queue's gate is bypassed because events are not pre-queued).

import (
	"context"
	"fmt"
	"time"
)

// StreamEvent is one event delivered by a streaming source, carrying the Event
// to dispatch. A source closes its channel to signal end-of-stream (a clean
// drain); ctx cancellation stops the loop at an event boundary.
type StreamEvent struct {
	Event Event
}

// StreamSource yields events over time. Events() returns a receive-only channel
// the loop drains until it is closed (clean end-of-stream) or ctx is cancelled.
// Implementations MUST deliver events in non-decreasing TsEvent order per the
// streaming contract; the loop enforces it and aborts on a reversal.
//
// A live source (moomoo Qot_UpdateKL push) closes its channel on graceful
// shutdown; a virtual-clock test source closes it after the last scripted bar.
type StreamSource interface {
	// Events returns the delivery channel. Called once by the loop.
	Events() <-chan StreamEvent
}

// streamClock is the clock seam the streaming loop drives. WallClock satisfies
// it trivially (Set is a no-op: real time advances itself); VirtualClock's Set
// moves event time forward. SimClock is NOT used here (the batch loop owns it).
type streamClock interface {
	Clock
	// set advances the clock to t for the virtual discipline; a WallClock
	// implements it as a no-op.
	set(t time.Time)
}

// wallStreamClock adapts WallClock to streamClock (Set is a no-op — wall time
// advances on its own).
type wallStreamClock struct{ WallClock }

func (wallStreamClock) set(time.Time) {}

// virtualStreamClock adapts *VirtualClock to streamClock (Set moves event time).
type virtualStreamClock struct{ *VirtualClock }

func (c virtualStreamClock) set(t time.Time) { c.VirtualClock.Set(t) }

// StreamClockMode selects the streaming clock discipline.
type StreamClockMode uint8

const (
	// StreamWall follows real time (live engine). The loop does not advance the
	// clock; handlers read host wall time.
	StreamWall StreamClockMode = iota
	// StreamVirtual follows event time (deterministic test). The loop sets the
	// clock to each event's timestamp before dispatch.
	StreamVirtual
)

// StreamLoop is the online event loop. Build with NewStreamLoop, register a
// handler per kind, then RunStream over a source. Like Loop it is single-
// goroutine: handlers run synchronously on the RunStream goroutine and may
// schedule same-or-later follow-on events via Schedule, which are drained
// before the next source event (so a bar's same-ts fills settle before the
// next bar — identical to the batch loop's intra-timestamp ordering).
type StreamLoop struct {
	clock    streamClock
	queue    *eventQueue // holds handler-scheduled follow-on events
	handlers map[EventKind]Handler
	lastTs   time.Time
	haveLast bool
	closed   bool
}

// NewStreamLoop returns a streaming loop using the given clock discipline. For
// StreamVirtual, vc must be non-nil (the controllable clock the test drives);
// for StreamWall, vc is ignored and a real WallClock is used.
func NewStreamLoop(mode StreamClockMode, vc *VirtualClock) *StreamLoop {
	var clk streamClock
	switch mode {
	case StreamVirtual:
		if vc == nil {
			vc = NewVirtualClock(time.Time{})
		}
		clk = virtualStreamClock{vc}
	default:
		clk = wallStreamClock{NewWallClock()}
	}
	return &StreamLoop{
		clock:    clk,
		queue:    newEventQueue(),
		handlers: make(map[EventKind]Handler),
	}
}

// Clock returns the loop's clock (read-only use by handlers).
func (l *StreamLoop) Clock() Clock { return l.clock }

// Now returns the loop's current time.
func (l *StreamLoop) Now() time.Time { return l.clock.Now() }

// Register installs the handler for a kind, replacing any previous one. Call
// before RunStream.
func (l *StreamLoop) Register(kind EventKind, h Handler) { l.handlers[kind] = h }

// NextSeq returns a fresh deterministic sequence token (for client-order ids
// etc.), consistent with the batch loop's source.
func (l *StreamLoop) NextSeq() uint64 { return l.queue.nextSeq() }

// Schedule enqueues a handler-produced follow-on event (same-ts or later),
// drained before the next source event. It rejects non-UTC timestamps, events
// before the current clock, and scheduling after close.
func (l *StreamLoop) Schedule(ev Event) (uint64, error) {
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

// RunStream drains src until its channel closes (clean end-of-stream) or ctx is
// cancelled. For each source event it:
//
//  1. validates UTC + non-decreasing TsEvent (a reversal aborts the run);
//  2. (virtual discipline) advances the clock to the event's timestamp;
//  3. dispatches the event to its registered handler;
//  4. drains any same-or-later follow-on events the handler scheduled, in the
//     deterministic queue order, BEFORE pulling the next source event.
//
// On return the loop is closed (no further Schedule). Returns nil on a clean
// drain, ctx.Err() on cancellation, or the first handler/dispatch error.
func (l *StreamLoop) RunStream(ctx context.Context, src StreamSource) (err error) {
	defer func() { l.closed = true }()
	ch := src.Events()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case se, ok := <-ch:
			if !ok {
				return nil // clean end-of-stream
			}
			if derr := l.dispatch(ctx, se.Event); derr != nil {
				return derr
			}
			// Drain handler-scheduled follow-ons (e.g. fills, samples) before the
			// next source event, preserving intra-timestamp ordering.
			if derr := l.drainQueue(ctx); derr != nil {
				return derr
			}
		}
	}
}

// dispatch validates, advances the clock (virtual) and routes one event.
func (l *StreamLoop) dispatch(ctx context.Context, ev Event) error {
	ts := ev.TsEvent()
	if _, off := ts.Zone(); off != 0 {
		return fmt.Errorf("%w: stream event %s at non-UTC %s", ErrTimeReversal, ev.Kind(), ts)
	}
	if l.haveLast && ts.Before(l.lastTs) {
		return fmt.Errorf("%w: stream delivered %s at %s after %s", ErrTimeReversal, ev.Kind(), ts, l.lastTs)
	}
	l.lastTs = ts
	l.haveLast = true
	// Virtual discipline: follow event time. Wall discipline: no-op (real time).
	l.clock.set(ts)
	h, ok := l.handlers[ev.Kind()]
	if !ok {
		return fmt.Errorf("stream: no handler registered for event kind %s", ev.Kind())
	}
	if herr := h.Handle(ctx, ev); herr != nil {
		return fmt.Errorf("stream handling %s at %s: %w", ev.Kind(), ts.Format(time.RFC3339Nano), herr)
	}
	return nil
}

// drainQueue dispatches all follow-on events the last handler scheduled, in
// total order, until the queue empties. Cancellation is checked per event.
func (l *StreamLoop) drainQueue(ctx context.Context) error {
	for {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		ev, ok := l.queue.pop()
		if !ok {
			return nil
		}
		if derr := l.dispatch(ctx, ev); derr != nil {
			return derr
		}
	}
}

// compile-time checks: the live clocks satisfy Clock, and the stream clocks
// satisfy the internal streamClock seam.
var (
	_ Clock        = WallClock{}
	_ Clock        = (*VirtualClock)(nil)
	_ streamClock  = wallStreamClock{}
	_ streamClock  = virtualStreamClock{}
	_ StreamSource = (*ChannelSource)(nil)
)

// ChannelSource is a simple StreamSource backed by a channel the producer
// writes to and closes. It is the seam both the moomoo live feed and the
// deterministic test feed implement: the producer goroutine pushes StreamEvents
// and closes the channel at end-of-stream.
type ChannelSource struct {
	ch <-chan StreamEvent
}

// NewChannelSource wraps a channel as a StreamSource.
func NewChannelSource(ch <-chan StreamEvent) *ChannelSource {
	return &ChannelSource{ch: ch}
}

// Events returns the underlying channel.
func (s *ChannelSource) Events() <-chan StreamEvent { return s.ch }
