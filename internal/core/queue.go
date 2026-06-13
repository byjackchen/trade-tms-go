package core

// queue.go implements the deterministic event queue: a binary min-heap whose
// total order is (TsEvent UTC, KindPriority, Seq). Seq is a strictly monotonic
// insertion counter owned by the queue, which makes the order a STRICT TOTAL
// order (no two queued items compare equal) and therefore fully deterministic
// regardless of heap internals.
//
// The implementation is a hand-rolled heap rather than container/heap to keep
// the comparator inlined, avoid interface boxing of indices, and make the
// total order auditable in one place.

import "time"

// queuedEvent is the heap element: the event plus its assigned sequence and a
// denormalized sort key cached at push time (timestamp unix-nanos + kind).
type queuedEvent struct {
	ev   Event
	tsNs int64  // TsEvent().UnixNano(), cached
	prio uint8  // Kind().Priority(), cached
	seq  uint64 // strictly monotonic insertion sequence
}

// less reports whether a must dispatch before b under the total order.
func (a queuedEvent) less(b queuedEvent) bool {
	if a.tsNs != b.tsNs {
		return a.tsNs < b.tsNs
	}
	if a.prio != b.prio {
		return a.prio < b.prio
	}
	return a.seq < b.seq
}

// eventQueue is a min-heap of queuedEvent. The zero value is not usable; build
// one with newEventQueue. Not safe for concurrent use.
type eventQueue struct {
	heap   []queuedEvent
	nextID uint64 // next Seq to assign; also the engine's deterministic id source
}

// newEventQueue returns an empty queue.
func newEventQueue() *eventQueue { return &eventQueue{} }

// Len reports the number of queued events.
func (q *eventQueue) Len() int { return len(q.heap) }

// nextSeq returns and consumes the next monotonic sequence value. It is the
// single deterministic seq/id source for the engine (never time- or
// random-derived).
func (q *eventQueue) nextSeq() uint64 {
	s := q.nextID
	q.nextID++
	return s
}

// push inserts ev, assigning it the next sequence, and restores the heap
// invariant. Returns the assigned sequence.
func (q *eventQueue) push(ev Event) uint64 {
	seq := q.nextSeq()
	item := queuedEvent{
		ev:   ev,
		tsNs: ev.TsEvent().UTC().UnixNano(),
		prio: ev.Kind().Priority(),
		seq:  seq,
	}
	q.heap = append(q.heap, item)
	q.siftUp(len(q.heap) - 1)
	return seq
}

// peekTs returns the timestamp of the next event without removing it, and ok.
func (q *eventQueue) peekTs() (ts time.Time, ok bool) {
	if len(q.heap) == 0 {
		return time.Time{}, false
	}
	return q.heap[0].ev.TsEvent(), true
}

// pop removes and returns the minimum event, and ok=false when empty.
func (q *eventQueue) pop() (Event, bool) {
	n := len(q.heap)
	if n == 0 {
		return nil, false
	}
	top := q.heap[0]
	last := q.heap[n-1]
	q.heap[0] = last
	q.heap[n-1] = queuedEvent{} // release reference for GC
	q.heap = q.heap[:n-1]
	if len(q.heap) > 0 {
		q.siftDown(0)
	}
	return top.ev, true
}

func (q *eventQueue) siftUp(i int) {
	for i > 0 {
		parent := (i - 1) / 2
		if !q.heap[i].less(q.heap[parent]) {
			break
		}
		q.heap[i], q.heap[parent] = q.heap[parent], q.heap[i]
		i = parent
	}
}

func (q *eventQueue) siftDown(i int) {
	n := len(q.heap)
	for {
		left := 2*i + 1
		if left >= n {
			break
		}
		smallest := left
		if right := left + 1; right < n && q.heap[right].less(q.heap[left]) {
			smallest = right
		}
		if !q.heap[smallest].less(q.heap[i]) {
			break
		}
		q.heap[i], q.heap[smallest] = q.heap[smallest], q.heap[i]
		i = smallest
	}
}
