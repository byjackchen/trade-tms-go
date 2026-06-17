package sepa

// incstate.go holds the per-generator incremental indicator state that replaces
// the O(window)-per-bar BATCH recomputation in the flat-book entry chain
// (maybeEnter -> ClassifyStage + EvaluateTrendTemplate). [PERF, agreement-critical]
//
// BATCH-AGREEMENT CONTRACT [HARD]: every value produced here is BYTE-IDENTICAL
// to the batch form it replaces. The trend-template / stage / 52wk logic compares MA
// values with STRICT inequalities, so a single-ULP drift could flip a boolean
// and change which signal fires. We therefore do NOT use a drifting running-sum
// SMA (internal/indicators.RollingSMA keeps an incrementally maintained sum that
// is only 1e-9-close to batch). Instead each MA value is a FRESH summation over
// the trailing `window` closes, in the SAME ascending index order as the batch
// indicators.SMA — bit-for-bit identical, but O(window) instead of the batch
// O(n*window) (the batch builds the whole length-n output slice every bar and
// SEPA reads only the tail). The 52wk high/low reuse the bit-exact monotonic-
// deque accumulators (indicators.RollingMaxAcc/RollingMinAcc), which the golden
// incremental_test proves are bit-identical to batch RollingMax/RollingMin.
//
// The maintained MA200 SERIES (one fresh-sum value appended per bar) backs the
// stage classifier's MASlopePct(200,20) lag read and FractionAbove(...,200)
// without recomputing the full batch MA(close,200) three times per bar.

import (
	"math"

	"github.com/byjackchen/trade-tms-go/internal/indicators"
)

// incState carries the streaming indicator state for one generator. It is fed
// once per bar in appendBar (AFTER the new close is appended), and is reset /
// rebuilt by WarmupFromHistory and LoadState which repopulate the buffer in
// bulk.
type incState struct {
	// 52-week high/low accumulators (window 252), bit-exact vs RollingMax/Min.
	// The accumulators expose only Update (no Value getter); we cache the last
	// Update return — the current 252-window rolling max/min (NaN during warmup).
	high252    *indicators.RollingMaxAcc
	low252     *indicators.RollingMinAcc
	high252Val float64
	low252Val  float64
	barsFed    int // total bars fed since last reset (for the <252 fallback)

	// Full-history extrema for the FiftyTwoWeekHigh/Low fallback used while the
	// 252-window is not yet full (batch falls back to Max(high)/Min(low) over the
	// WHOLE current buffer). We track these over the live buffer; because the
	// buffer only ever reaches the 1000-bar cap (>=252) before any trim, the
	// fallback branch is only ever taken while the buffer is still growing and
	// never trimmed, so a simple running extover-the-buffer is exact.
	fullHigh float64 // max of all highs currently in the buffer (NaN if empty)
	fullLow  float64 // min of all lows currently in the buffer (NaN if empty)

	// MA200 series parallel to the close buffer (same length, same front-trim).
	// ma200[i] == batch SMA(close,200)[i] for i in the non-warmup region; the
	// first <200 entries of the buffer are NaN (warmup), matching batch. Only the
	// trailing ~221 entries are ever read (FractionAbove last 200, slope lag 20),
	// all of which are non-warmup once len(close) >= 220.
	ma200 []float64
}

func newIncState() *incState {
	return &incState{
		high252:    indicators.NewRollingMax(indicators.TTHighLowWindow),
		low252:     indicators.NewRollingMin(indicators.TTHighLowWindow),
		high252Val: indicators.NaN,
		low252Val:  indicators.NaN,
		fullHigh:   indicators.NaN,
		fullLow:    indicators.NaN,
	}
}

// freshSMA computes SMA(close, window) at the LAST index of `close`, bit-for-bit
// identical to indicators.SMA(close, window)[len-1]: a fresh ascending summation
// over close[n-window .. n-1]. Returns NaN during warmup (n < window) or when the
// window contains a NaN (batch NaN propagation).
func freshSMA(close []float64, window int) float64 {
	n := len(close)
	if n < window {
		return indicators.NaN
	}
	sum := 0.0
	for j := n - window; j < n; j++ {
		v := close[j]
		if math.IsNaN(v) {
			return indicators.NaN
		}
		sum += v
	}
	return sum / float64(window)
}

// onAppend updates the streaming state after one bar (high/low/close) has been
// appended to the generator's buffer. `close` is the post-append close buffer
// (so close[len-1] is the new bar). It feeds the 252 high/low accumulators and
// appends the new MA200 value.
func (s *incState) onAppend(high, low float64, close []float64) {
	s.high252Val = s.high252.Update(high)
	s.low252Val = s.low252.Update(low)
	s.barsFed++

	if math.IsNaN(s.fullHigh) || high > s.fullHigh {
		s.fullHigh = high
	}
	if math.IsNaN(s.fullLow) || low < s.fullLow {
		s.fullLow = low
	}

	s.ma200 = append(s.ma200, freshSMA(close, 200))
}

// trimFront drops `cut` leading MA200 entries in lockstep with the close buffer
// front-trim. Reslicing (no copy) keeps it allocation-free.
func (s *incState) trimFront(cut int) {
	if cut > 0 && cut <= len(s.ma200) {
		s.ma200 = s.ma200[cut:]
	}
}

// rebuild repopulates the streaming state from scratch over the given buffers
// (oldest first). Used by WarmupFromHistory / LoadState which set the buffer in
// bulk. Produces state byte-identical to feeding the bars one-by-one.
func (s *incState) rebuild(high, low, close []float64) {
	s.high252 = indicators.NewRollingMax(indicators.TTHighLowWindow)
	s.low252 = indicators.NewRollingMin(indicators.TTHighLowWindow)
	s.high252Val = indicators.NaN
	s.low252Val = indicators.NaN
	s.barsFed = 0
	s.fullHigh = indicators.NaN
	s.fullLow = indicators.NaN
	s.ma200 = make([]float64, 0, len(close))
	for i := range close {
		s.high252Val = s.high252.Update(high[i])
		s.low252Val = s.low252.Update(low[i])
		s.barsFed++
		if math.IsNaN(s.fullHigh) || high[i] > s.fullHigh {
			s.fullHigh = high[i]
		}
		if math.IsNaN(s.fullLow) || low[i] < s.fullLow {
			s.fullLow = low[i]
		}
		s.ma200 = append(s.ma200, freshSMA(close[:i+1], 200))
	}
}

// fiftyTwoWeekHigh returns FiftyTwoWeekHigh(high, 252) bit-identically: the
// 252-window rolling max last value, or Max(whole buffer) while the window is
// not yet full (batch fallback).
func (s *incState) fiftyTwoWeekHigh() float64 {
	if s.barsFed >= indicators.TTHighLowWindow {
		return s.high252Val
	}
	return s.fullHigh
}

func (s *incState) fiftyTwoWeekLow() float64 {
	if s.barsFed >= indicators.TTHighLowWindow {
		return s.low252Val
	}
	return s.fullLow
}
