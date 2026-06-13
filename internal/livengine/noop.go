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
type NoopExecutor struct {
	seq atomic.Uint64
	// submitted counts orders the strategies WOULD have placed (telemetry only;
	// signal mode never executes them). Useful for the cockpit "intents vs would-
	// be-orders" panel and for asserting strategies actually fired in tests.
	submitted atomic.Int64
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
// placed, no fill produced, no account mutated (signal mode).
func (e *NoopExecutor) SubmitMarket(_ string, _ string, _ domain.OrderSide, _ domain.Qty, _ string, _ time.Time) (string, error) {
	e.submitted.Add(1)
	return e.nextID(), nil
}

// SubmitMarketSignal records a would-be signal order. The portfolio gate is NOT
// run here (the live session runs the gate explicitly when it wants gate audit;
// in signal mode the gate decision is informational, decision 6). Returns
// submitted=true so the strategy adapter proceeds exactly as in backtest (the
// adapter only reads the bool to decide whether to keep going; nothing is
// actually placed).
func (e *NoopExecutor) SubmitMarketSignal(_ string, _ string, _ domain.SignalSide, _ domain.OrderSide, _ domain.Qty, _ string, _ time.Time) (string, bool, error) {
	e.submitted.Add(1)
	return e.nextID(), true, nil
}

// NetPosition always returns flat (0): signal mode holds no positions, so a
// strategy's FLAT-close sizing yields a no-op close (decision 6).
func (e *NoopExecutor) NetPosition(_ string, _ string) domain.Qty { return 0 }

// WouldSubmitCount returns how many orders the strategies attempted to submit
// (telemetry; signal mode placed none).
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
