package core

// event.go defines the Event interface and the concrete event kinds the
// engine schedules, plus the fixed kind-priority ordering that, together with
// the per-event timestamp and insertion sequence, gives the queue a strict
// total order (see doc.go).

import (
	"time"

	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// EventKind is a small enumeration used purely for the deterministic
// tie-break between events sharing a timestamp. Lower values dispatch first.
//
// The ordering is the engine's own main-loop contract for a data point at
// timestamp T: the exchange ingests the bar, strategies' on_bar fire, then
// venues settle. The bar's arrival drives both the exchange book update
// (inside the executor) and the strategy callback in one KindBar dispatch;
// fills the executor produces are scheduled as KindFill at the SAME timestamp
// but a LATER kind-priority, so accounting observes them after every strategy
// at that timestamp has seen the bar.
type EventKind uint8

const (
	// KindBar delivers one bar to the executor (book update) and strategies
	// (on_bar). Highest priority: market data leads each timestamp.
	KindBar EventKind = iota
	// KindFill settles an execution: it mutates positions/account and emits
	// account-state. Fills are produced during KindBar dispatch and scheduled
	// at the same timestamp, after all bars, so the close-price fill of a
	// market order submitted in on_bar(T) settles within timestamp T
	// (same-bar-close fills).
	KindFill
	// KindSample triggers per-bar sampling (equity curve). Lowest priority:
	// samplers observe the fully-settled state at the end of a timestamp.
	KindSample
)

// Priority returns the dispatch priority of the kind (lower first). It is the
// second key of the total order, after TsEvent and before Seq.
func (k EventKind) Priority() uint8 { return uint8(k) }

// String renders the kind for logs and test failures.
func (k EventKind) String() string {
	switch k {
	case KindBar:
		return "bar"
	case KindFill:
		return "fill"
	case KindSample:
		return "sample"
	default:
		return "unknown"
	}
}

// Event is one scheduled occurrence in the simulated timeline. Implementations
// are small immutable value carriers; the loop dispatches them by Kind.
//
// TsEvent must be UTC. The queue rejects an event whose TsEvent is before the
// clock (time never moves backwards), and the loop advances the clock to
// TsEvent before dispatching.
type Event interface {
	// TsEvent is the UTC timestamp at which the event occurs.
	TsEvent() time.Time
	// Kind is the dispatch class and tie-break priority.
	Kind() EventKind
}

// ---------------------------------------------------------------------------
// BarEvent
// ---------------------------------------------------------------------------

// BarEvent carries one bar to the engine. Bar is the domain bar; Seq within a
// timestamp is assigned by the queue and reflects instrument registration
// order (the assembler enqueues per-instrument bars in registration order).
type BarEvent struct {
	Bar domain.Bar
}

// TsEvent returns the bar's UTC timestamp.
func (e BarEvent) TsEvent() time.Time { return e.Bar.TS }

// Kind returns KindBar.
func (e BarEvent) Kind() EventKind { return KindBar }

// ---------------------------------------------------------------------------
// FillEvent
// ---------------------------------------------------------------------------

// FillEvent carries one execution back into the loop for settlement. The
// executor produces it during KindBar dispatch and schedules it at the bar's
// timestamp; accounting consumes it on KindFill dispatch.
type FillEvent struct {
	Fill domain.Fill
}

// TsEvent returns the fill's UTC timestamp.
func (e FillEvent) TsEvent() time.Time { return e.Fill.TS }

// Kind returns KindFill.
func (e FillEvent) Kind() EventKind { return KindFill }

// ---------------------------------------------------------------------------
// SampleEvent
// ---------------------------------------------------------------------------

// SampleEvent triggers end-of-timestamp sampling (e.g. the equity-curve
// sampler) after all bars and fills at TS have settled.
type SampleEvent struct {
	TS time.Time
}

// TsEvent returns the sample timestamp.
func (e SampleEvent) TsEvent() time.Time { return e.TS }

// Kind returns KindSample.
func (e SampleEvent) Kind() EventKind { return KindSample }
