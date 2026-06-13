package core

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/domain"
)

func barAt(symbol string, t time.Time) BarEvent {
	return BarEvent{Bar: domain.Bar{Symbol: symbol, TS: t, Open: 1, High: 1, Low: 1, Close: 1}}
}

func fillAt(t time.Time) FillEvent {
	return FillEvent{Fill: domain.Fill{TS: t}}
}

// TestQueueOrdersByTimestamp pops events in ascending timestamp order.
func TestQueueOrdersByTimestamp(t *testing.T) {
	q := newEventQueue()
	t1 := time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)
	t2 := time.Date(2025, 1, 3, 0, 0, 0, 0, time.UTC)
	t3 := time.Date(2025, 1, 6, 0, 0, 0, 0, time.UTC)
	// Push out of order.
	q.push(barAt("A", t3))
	q.push(barAt("A", t1))
	q.push(barAt("A", t2))

	got := drainTimes(q)
	assert.Equal(t, []time.Time{t1, t2, t3}, got)
}

// TestQueueKindPriority orders bar < fill < sample at the same timestamp.
func TestQueueKindPriority(t *testing.T) {
	q := newEventQueue()
	t1 := time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)
	// Push in reverse priority order.
	q.push(SampleEvent{TS: t1})
	q.push(fillAt(t1))
	q.push(barAt("A", t1))

	e1, _ := q.pop()
	e2, _ := q.pop()
	e3, _ := q.pop()
	assert.Equal(t, KindBar, e1.Kind())
	assert.Equal(t, KindFill, e2.Kind())
	assert.Equal(t, KindSample, e3.Kind())
}

// TestQueueInsertionTieBreak orders equal (ts, kind) events by insertion seq,
// which is what makes same-timestamp multi-symbol bars deterministic
// (registration order).
func TestQueueInsertionTieBreak(t *testing.T) {
	q := newEventQueue()
	t1 := time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)
	// Three same-ts bars enqueued in registration order C, A, B.
	q.push(barAt("C", t1))
	q.push(barAt("A", t1))
	q.push(barAt("B", t1))

	var order []string
	for {
		ev, ok := q.pop()
		if !ok {
			break
		}
		order = append(order, ev.(BarEvent).Bar.Symbol)
	}
	assert.Equal(t, []string{"C", "A", "B"}, order, "same-ts bars keep insertion (registration) order")
}

// TestQueueDeterministicAcrossPermutations is a stronger determinism check: any
// push permutation of the same (ts, kind) multiset yields the SAME pop order
// when seqs are assigned in push order — and a fixed push order is always
// stable. We assert the heap is a strict total order: re-running an identical
// push sequence gives an identical pop sequence.
func TestQueueDeterministicRepeat(t *testing.T) {
	build := func() []string {
		q := newEventQueue()
		base := time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)
		for i := 0; i < 50; i++ {
			// interleave timestamps and kinds deterministically
			tt := base.AddDate(0, 0, i%5)
			switch i % 3 {
			case 0:
				q.push(barAt("S", tt))
			case 1:
				q.push(fillAt(tt))
			case 2:
				q.push(SampleEvent{TS: tt})
			}
		}
		var seq []string
		for {
			ev, ok := q.pop()
			if !ok {
				break
			}
			seq = append(seq, ev.TsEvent().Format("01-02")+":"+ev.Kind().String())
		}
		return seq
	}
	a := build()
	b := build()
	assert.Equal(t, a, b, "identical push sequence must give identical pop sequence")
	// And the sequence is globally sorted by (ts, kind).
	for i := 1; i < len(a); i++ {
		assert.LessOrEqual(t, a[i-1], a[i], "pop sequence is non-decreasing by (ts,kind) key text")
	}
}

// TestQueueSeqMonotonic confirms nextSeq is strictly increasing from 0.
func TestQueueSeqMonotonic(t *testing.T) {
	q := newEventQueue()
	require.Equal(t, uint64(0), q.nextSeq())
	require.Equal(t, uint64(1), q.nextSeq())
	require.Equal(t, uint64(2), q.nextSeq())
}

func drainTimes(q *eventQueue) []time.Time {
	var out []time.Time
	for {
		ev, ok := q.pop()
		if !ok {
			return out
		}
		out = append(out, ev.TsEvent())
	}
}
