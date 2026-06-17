package livengine

// noop.go is the signal-mode executor: an engine.OrderSubmitter (+ PositionReader)
// that records NO orders, submits NO fills and mutates NO account — it only
// assigns deterministic client-order ids so a strategy's OnBar runs to
// completion exactly as in backtest. In signal mode there are no positions, so
// NetPosition always reports flat (0), which makes a strategy's FLAT-close
// branch a no-op (decision 3 + 6) — the right behaviour for "record what the
// strategy is thinking, place nothing".
//
// Crucially this does NOT change strategy INTERNAL state: the generators evolve
// purely from OnBar(bar) inputs and never read fills/positions, so the
// SignalIntents the session evaluates after each bar are identical to the batch
// path's (consistency_test.go). Only the (discarded) order side differs.

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// NoopExecutor satisfies engine.OrderSubmitter and engine.PositionReader for the
// signal-mode live engine. It places nothing; it exists so real strategy
// adapters (which call sub.SubmitMarket / sub.SubmitMarketSignal / read
// NetPosition) run unmodified. seq is a deterministic monotonic id source
// (atomic so a future concurrent feed is safe; the single-goroutine loop is the
// common case).
//
// Each would-be order is CAPTURED as an IntentRecord (concept B: side+qty) into
// pending; the session DRAINS pending at each timestamp's flush and emits them
// to the IntentSink, PARALLEL to signal emission. WouldSubmitCount stays as a
// convenience counter (the cockpit "intent count").
type NoopExecutor struct {
	seq atomic.Uint64
	// submitted counts orders the strategies WOULD have placed (the intent count;
	// signal mode never executes them). Useful for the cockpit intent panel and
	// for asserting strategies actually fired in tests.
	submitted atomic.Int64
	// pending holds the would-be order intents captured since the last drain, in
	// capture (OnBar) order. The single-goroutine dispatch loop appends here; the
	// session's flushTimestamp drains it. mu guards it because the telemetry
	// counters above are read concurrently and a future concurrent feed is allowed.
	mu      sync.Mutex
	pending []IntentRecord
}

// NewNoopExecutor returns a fresh signal-mode executor.
func NewNoopExecutor() *NoopExecutor { return &NoopExecutor{} }

// nextID returns the next deterministic client-order id (a zero-padded counter),
// mirroring the engine's NextClientOrderID scheme closely enough for telemetry;
// the id is never persisted as an order in signal mode.
func (e *NoopExecutor) nextID() string {
	n := e.seq.Add(1) - 1
	return "SIGNAL-" + formatSeq(n)
}

// SubmitMarket records a would-be market order and returns its id. No order is
// placed, no fill produced, no account mutated (signal mode). The order is
// captured as an IntentRecord; SubmitMarket is the UNGATED close primitive and
// carries only a broker OrderSide, so the intent Side is mapped back to a
// SignalSide (BUY→LONG, SELL→SHORT).
func (e *NoopExecutor) SubmitMarket(strategyID, symbol string, side domain.OrderSide, qty domain.Qty, _ string, ts time.Time) (string, error) {
	e.capture(IntentRecord{StrategyID: strategyID, AsOf: ts, Symbol: symbol, Side: signalSideOf(side), Qty: qty})
	return e.nextID(), nil
}

// SubmitMarketSignal records a would-be signal order. The portfolio gate is NOT
// run here (the live session runs the gate explicitly when it wants gate audit;
// in signal mode the gate decision is informational, decision 6). Returns
// submitted=true so the strategy adapter proceeds exactly as in backtest (the
// adapter only reads the bool to decide whether to keep going; nothing is
// actually placed). The order is captured as an IntentRecord carrying the
// strategy-level SignalSide directly (the faithful intent direction, incl. FLAT).
func (e *NoopExecutor) SubmitMarketSignal(strategyID, symbol string, signalSide domain.SignalSide, _ domain.OrderSide, qty domain.Qty, _ string, ts time.Time) (string, bool, error) {
	e.capture(IntentRecord{StrategyID: strategyID, AsOf: ts, Symbol: symbol, Side: signalSide, Qty: qty})
	return e.nextID(), true, nil
}

// capture records one would-be order intent (bumps the count + buffers the
// record for the session to drain).
func (e *NoopExecutor) capture(rec IntentRecord) {
	e.submitted.Add(1)
	e.mu.Lock()
	e.pending = append(e.pending, rec)
	e.mu.Unlock()
}

// drainPending returns the would-be order intents captured since the last drain
// (in capture order) and clears the buffer. The session calls it at each
// timestamp's flush to emit them to the IntentSink.
func (e *NoopExecutor) drainPending() []IntentRecord {
	e.mu.Lock()
	out := e.pending
	e.pending = nil
	e.mu.Unlock()
	return out
}

// signalSideOf maps a broker OrderSide back to the strategy-level SignalSide for
// the ungated SubmitMarket primitive (BUY→LONG, SELL→SHORT). SubmitMarketSignal
// carries the true SignalSide (incl. FLAT) directly and does not use this.
func signalSideOf(side domain.OrderSide) domain.SignalSide {
	if side == domain.OrderSideSell {
		return domain.SideShort
	}
	return domain.SideLong
}

// NetPosition always returns flat (0): signal mode holds no positions, so a
// strategy's FLAT-close sizing yields a no-op close (decision 6).
func (e *NoopExecutor) NetPosition(_ string, _ string) domain.Qty { return 0 }

// WouldSubmitCount returns the intent count: how many would-be orders the
// strategies attempted to submit (convenience counter; signal mode placed none,
// it captured each as an IntentRecord instead).
func (e *NoopExecutor) WouldSubmitCount() int64 { return e.submitted.Load() }

// formatSeq renders n as a zero-padded decimal (stable width for sortable ids).
func formatSeq(n uint64) string {
	const width = 9
	buf := make([]byte, 0, width)
	digits := make([]byte, 0, 20)
	if n == 0 {
		digits = append(digits, '0')
	}
	for n > 0 {
		digits = append(digits, byte('0'+n%10))
		n /= 10
	}
	for len(digits) < width {
		digits = append(digits, '0')
	}
	for i := len(digits) - 1; i >= 0; i-- {
		buf = append(buf, digits[i])
	}
	return string(buf)
}
