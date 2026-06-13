package indicators

import "math"

// This file provides streaming (incremental) counterparts to the batch rolling
// primitives in rolling.go. Strategy signal generators feed bars one at a time
// via on_bar(); these accumulators give O(1)-amortized updates that produce
// EXACTLY the same value the batch form would compute over the trailing window
// (verified bit-for-bit in the golden tests).
//
// Every Update returns the indicator value AFTER ingesting the new sample.
// Before the window is full the return is NaN (warmup), matching pandas
// min_periods == window. NaN samples are stored verbatim so window membership
// stays aligned with the batch form; a window containing any NaN returns NaN.

// RollingSMA is a streaming simple moving average with an O(1) update.
type RollingSMA struct {
	window int
	buf    []float64
	idx    int // next write position (ring buffer)
	count  int // samples seen (capped semantics below)
	sum    float64
	nans   int // count of NaN currently inside the window
}

// NewRollingSMA constructs an SMA accumulator over the given window. Panics if
// window <= 0.
func NewRollingSMA(window int) *RollingSMA {
	if window <= 0 {
		panic("indicators: NewRollingSMA window must be > 0")
	}
	return &RollingSMA{window: window, buf: make([]float64, window)}
}

// Update ingests one sample and returns the current SMA (NaN during warmup or
// when the window contains a NaN).
func (r *RollingSMA) Update(v float64) float64 {
	if r.count == r.window {
		old := r.buf[r.idx]
		if math.IsNaN(old) {
			r.nans--
		} else {
			r.sum -= old
		}
	} else {
		r.count++
	}
	r.buf[r.idx] = v
	if math.IsNaN(v) {
		r.nans++
	} else {
		r.sum += v
	}
	r.idx = (r.idx + 1) % r.window
	return r.Value()
}

// Value returns the current SMA without ingesting a sample.
func (r *RollingSMA) Value() float64 {
	if r.count < r.window || r.nans > 0 {
		return NaN
	}
	return r.sum / float64(r.window)
}

// RollingStdAcc is a streaming rolling std (sample, ddof configurable).
//
// It maintains a ring buffer of the trailing `window` samples and computes the
// std with the SAME two-pass mean/variance formulation as the batch RollingStd,
// so the streaming output is bit-for-bit identical to the batch (pandas-parity)
// form — no single-pass Σx² catastrophic-cancellation drift. The per-update
// cost is O(window); for the small windows strategies use (lookback ~60) this
// is negligible and buys exact parity, which matters more than micro-throughput.
type RollingStdAcc struct {
	window int
	ddof   int
	buf    []float64
	idx    int
	count  int
	nans   int
}

// NewRollingStd constructs a rolling std accumulator. ddof=1 matches pandas
// default (sample std); ddof=0 gives population std. Panics if window <= 0.
func NewRollingStd(window, ddof int) *RollingStdAcc {
	if window <= 0 {
		panic("indicators: NewRollingStd window must be > 0")
	}
	return &RollingStdAcc{window: window, ddof: ddof, buf: make([]float64, window)}
}

// Update ingests one sample and returns the current rolling std.
func (r *RollingStdAcc) Update(v float64) float64 {
	if r.count == r.window {
		if math.IsNaN(r.buf[r.idx]) {
			r.nans--
		}
	} else {
		r.count++
	}
	r.buf[r.idx] = v
	if math.IsNaN(v) {
		r.nans++
	}
	r.idx = (r.idx + 1) % r.window
	return r.Value()
}

// Value returns the current rolling std (NaN during warmup, when a NaN sits in
// the window, or when window-ddof <= 0). Two-pass over the live ring buffer,
// identical to the batch RollingStd.
func (r *RollingStdAcc) Value() float64 {
	denom := float64(r.window - r.ddof)
	if r.count < r.window || r.nans > 0 || denom <= 0 {
		return NaN
	}
	// Iterate the ring buffer in CHRONOLOGICAL order (oldest first) so the
	// summation order matches the batch form exactly (ULP-identical). When the
	// window is full, the oldest slot is r.idx (the next write position).
	sum := 0.0
	for k := 0; k < r.window; k++ {
		sum += r.buf[(r.idx+k)%r.window]
	}
	mean := sum / float64(r.window)
	ss := 0.0
	for k := 0; k < r.window; k++ {
		d := r.buf[(r.idx+k)%r.window] - mean
		ss += d * d
	}
	return math.Sqrt(ss / denom)
}

// monoDeque is a monotonic index deque backing the streaming min/max.
type monoDeque struct {
	window int
	// ring buffer of values for window-membership bookkeeping
	buf   []float64
	head  int // absolute index of the oldest stored sample
	count int64
	// dq holds absolute sample indices; values are monotonic per wantMax
	dq      []int64
	wantMax bool
	nans    int
}

func newMonoDeque(window int, wantMax bool) *monoDeque {
	return &monoDeque{
		window:  window,
		buf:     make([]float64, window),
		wantMax: wantMax,
	}
}

func (m *monoDeque) update(v float64) float64 {
	cur := m.count // absolute index of this new sample
	m.count++
	// Evict samples that fell out of the window from the front.
	lo := cur - int64(m.window) + 1
	// Track NaN count over the window using the ring buffer.
	slot := int(cur % int64(m.window))
	if m.count > int64(m.window) {
		// overwriting an old slot; adjust NaN count for the evicted value
		evicted := m.buf[slot]
		if math.IsNaN(evicted) {
			m.nans--
		}
	}
	m.buf[slot] = v
	if math.IsNaN(v) {
		m.nans++
	}
	_ = m.head
	// Maintain the monotonic deque only over non-NaN values; if v is NaN the
	// window result will be NaN anyway, so we still must keep indices in range.
	for len(m.dq) > 0 && m.dq[0] < lo {
		m.dq = m.dq[1:]
	}
	if !math.IsNaN(v) {
		for len(m.dq) > 0 {
			backIdx := m.dq[len(m.dq)-1]
			backVal := m.buf[int(backIdx%int64(m.window))]
			if math.IsNaN(backVal) {
				// shouldn't happen (we never push NaN), but guard
				m.dq = m.dq[:len(m.dq)-1]
				continue
			}
			if (m.wantMax && backVal <= v) || (!m.wantMax && backVal >= v) {
				m.dq = m.dq[:len(m.dq)-1]
			} else {
				break
			}
		}
		m.dq = append(m.dq, cur)
	}
	if m.count < int64(m.window) || m.nans > 0 {
		return NaN
	}
	if len(m.dq) == 0 {
		return NaN
	}
	return m.buf[int(m.dq[0]%int64(m.window))]
}

// RollingMaxAcc is a streaming rolling max (O(1) amortized via monotonic deque).
type RollingMaxAcc struct{ d *monoDeque }

// NewRollingMax constructs a streaming rolling max. Panics if window <= 0.
func NewRollingMax(window int) *RollingMaxAcc {
	if window <= 0 {
		panic("indicators: NewRollingMax window must be > 0")
	}
	return &RollingMaxAcc{d: newMonoDeque(window, true)}
}

// Update ingests a sample and returns the current rolling max.
func (r *RollingMaxAcc) Update(v float64) float64 { return r.d.update(v) }

// RollingMinAcc is a streaming rolling min.
type RollingMinAcc struct{ d *monoDeque }

// NewRollingMin constructs a streaming rolling min. Panics if window <= 0.
func NewRollingMin(window int) *RollingMinAcc {
	if window <= 0 {
		panic("indicators: NewRollingMin window must be > 0")
	}
	return &RollingMinAcc{d: newMonoDeque(window, false)}
}

// Update ingests a sample and returns the current rolling min.
func (r *RollingMinAcc) Update(v float64) float64 { return r.d.update(v) }
