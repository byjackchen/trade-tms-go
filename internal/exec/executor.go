package exec

// executor.go is the SimExecutor: it accepts order submissions from strategies
// during bar processing, simulates fills via the configured FillModel, and
// emits Fill events. It maintains the L1 "book" implicitly through the bar it
// processes (the close-fill model reads the bar close; the realistic model
// reads the next bar open).
//
// Order id assignment is deterministic: client order ids are derived from a
// monotonic counter seeded by the engine (never time/random based), so a rerun
// reproduces identical ids. Venue order ids and trade ids likewise.
//
// Threading: single-goroutine. The engine calls Submit during a bar's strategy
// callback and ProcessBar during that same bar's executor dispatch.

import (
	"fmt"

	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// FillSink receives fills the executor produces (the engine forwards them into
// the loop as FillEvents and on to accounting). Implementations must be
// synchronous and deterministic.
type FillSink interface {
	EmitFill(domain.Fill) error
}

// SeqSource yields monotonic deterministic sequence values for id generation.
type SeqSource interface {
	NextSeq() uint64
}

// SimExecutor simulates order execution against bars. Build with NewSimExecutor.
type SimExecutor struct {
	model FillModel
	sink  FillSink
	seq   SeqSource

	// pending holds orders awaiting a fill, keyed by symbol, for next-bar
	// models. For this-bar models it is unused (orders fill immediately within
	// ProcessBar of the submission bar).
	pending map[string][]domain.Order

	// submittedThisBar holds orders submitted during the CURRENT TIMESTAMP's
	// strategy callbacks, for this-bar models, in submission order, awaiting the
	// end-of-timestamp flush (FlushThisBar). Cross-symbol orders are supported:
	// an order for symbol X submitted while symbol Y's bar is dispatching fills
	// at X's close for THIS timestamp (X's bar was already observed earlier in
	// the same timestamp, registration order): a market order fills at the
	// instrument's latest bar close at the submission timestamp — not one bar
	// later. (A multi-leg strategy like
	// Pairs submits both legs while the 2nd leg's bar is dispatching; both legs
	// must fill at THIS timestamp's closes.)
	submittedThisBar []domain.Order

	// barsThisTS records each symbol's bar observed at the current timestamp, so
	// a flushed this-bar order fills against its own symbol's close at this ts.
	// Cleared when the timestamp advances.
	barsThisTS map[string]domain.Bar
	curTS      int64
	curTSSet   bool
}

// NewSimExecutor wires an executor to a fill model, a fill sink and a
// deterministic sequence source.
func NewSimExecutor(model FillModel, sink FillSink, seq SeqSource) *SimExecutor {
	return &SimExecutor{
		model:      model,
		sink:       sink,
		seq:        seq,
		pending:    make(map[string][]domain.Order),
		barsThisTS: make(map[string]domain.Bar),
	}
}

// Model returns the active fill model (for run metadata).
func (e *SimExecutor) Model() FillModel { return e.model }

// NewClientOrderID returns a deterministic client order id for a submission.
// Format: "O-<seq>" where seq is the engine's monotonic counter.
func (e *SimExecutor) NewClientOrderID() string {
	return fmt.Sprintf("O-%d", e.seq.NextSeq())
}

// Submit accepts an order during a bar's strategy callback. The order must be a
// validated MARKET order. For this-bar models it is queued for the current
// bar's ProcessBar; for next-bar models it is queued against the next bar for
// the symbol.
func (e *SimExecutor) Submit(order domain.Order) error {
	if err := order.Validate(); err != nil {
		return fmt.Errorf("executor submit: %w", err)
	}
	if order.Type != domain.OrderTypeMarket {
		return fmt.Errorf("%w: SimExecutor supports MARKET orders only (extension point for LIMIT/STOP), got %s",
			domain.ErrInvalidArgument, order.Type)
	}
	switch e.model.Timing() {
	case FillThisBar:
		// Accumulate in submission order; filled at the end-of-timestamp flush
		// (FlushThisBar) against each order's own symbol's bar at this ts.
		e.submittedThisBar = append(e.submittedThisBar, order)
	case FillNextBar:
		e.pending[order.Symbol] = append(e.pending[order.Symbol], order)
	default:
		return fmt.Errorf("executor: unknown fill timing %d", e.model.Timing())
	}
	return nil
}

// ProcessBar simulates execution for one bar. Call order within a bar's
// dispatch matters and is fixed by the engine:
//
//   - this-bar model: the engine records each bar (ProcessBar) as it dispatches,
//     then — AFTER all bars at a timestamp have dispatched and all strategy
//     on_bar callbacks have run — calls FlushThisBar(ts) once, filling every
//     order submitted during that timestamp against ITS OWN symbol's close at
//     that timestamp. This supports cross-symbol same-timestamp fills (a Pairs
//     leg submitted while the other leg's bar dispatches fills at THIS ts's
//     close, not one bar later).
//   - next-bar model: the engine calls ProcessBar(bar) FIRST (filling orders
//     pending from prior bars against this bar's open), THEN the strategy
//     on_bar (which Submits orders pending for the next bar).
//
// Fills are emitted via the sink in deterministic (submission) order. Returns
// the number of fills emitted (always 0 for the this-bar model — fills are
// emitted by FlushThisBar).
func (e *SimExecutor) ProcessBar(bar domain.Bar) (int, error) {
	switch e.model.Timing() {
	case FillThisBar:
		// Record this bar for the end-of-timestamp flush. When the timestamp
		// advances, reset the per-ts bar map (the prior ts was already flushed
		// by the engine before any new-ts bar dispatches).
		ts := bar.TS.UnixNano()
		if !e.curTSSet || ts != e.curTS {
			e.barsThisTS = make(map[string]domain.Bar)
			e.curTS = ts
			e.curTSSet = true
		}
		e.barsThisTS[bar.Symbol] = bar
		return 0, nil
	case FillNextBar:
		orders := e.pending[bar.Symbol]
		delete(e.pending, bar.Symbol)
		count := 0
		for _, order := range orders {
			if err := e.fill(order, bar); err != nil {
				return count, err
			}
			count++
		}
		return count, nil
	}
	return 0, nil
}

// FlushThisBar fills every order submitted during the current timestamp (this-bar
// model) against its own symbol's bar at this timestamp, in submission order. The
// engine calls it ONCE per timestamp, after all bars at that ts have dispatched
// and all strategy on_bar callbacks have run, so the full set of same-ts bars
// (across symbols) is available for cross-symbol fills. An order whose symbol had
// no bar at this ts (should not happen for a strategy that only trades on its own
// bars) is left pending and surfaces via PendingCount. No-op for the next-bar
// model. Returns the number of fills emitted.
func (e *SimExecutor) FlushThisBar() (int, error) {
	if e.model.Timing() != FillThisBar || len(e.submittedThisBar) == 0 {
		return 0, nil
	}
	orders := e.submittedThisBar
	e.submittedThisBar = nil
	count := 0
	var unfilled []domain.Order
	for _, order := range orders {
		bar, ok := e.barsThisTS[order.Symbol]
		if !ok {
			// No bar for this symbol at this ts: cannot fill at this ts's close.
			// Keep it for a later flush (defensive; not expected in practice).
			unfilled = append(unfilled, order)
			continue
		}
		if err := e.fill(order, bar); err != nil {
			return count, err
		}
		count++
	}
	e.submittedThisBar = unfilled
	return count, nil
}

// FillAtBar prices and emits fills for one order against a specific bar
// immediately (bypassing the per-timestamp queue), in leg order. The engine uses
// it for END-OF-RUN LIQUIDATION: after the last bar, a flattening market order
// per open net position fills against the instrument's last bar — there is no
// future bar, so the order cannot route through the normal this-bar/next-bar
// queue. Filling against the symbol's LAST bar (real close + volume) applies the
// same depth-walk (close-fill) or slippage/commission (realistic) the
// in-loop path applies. The order must be a validated MARKET order.
func (e *SimExecutor) FillAtBar(order domain.Order, bar domain.Bar) error {
	if err := order.Validate(); err != nil {
		return fmt.Errorf("executor liquidation fill: %w", err)
	}
	if order.Type != domain.OrderTypeMarket {
		return fmt.Errorf("%w: SimExecutor supports MARKET orders only, got %s",
			domain.ErrInvalidArgument, order.Type)
	}
	return e.fill(order, bar)
}

// fill prices the order against bar (possibly across several price legs) and
// emits one domain.Fill per leg, in leg order. Each leg gets the same venue
// order id (one order) and a distinct trade id (leg index appended): one trade
// id per execution.
func (e *SimExecutor) fill(order domain.Order, bar domain.Bar) error {
	legs, err := e.model.Fill(order, bar)
	if err != nil {
		return fmt.Errorf("pricing order %s: %w", order.ClientOrderID, err)
	}
	if len(legs) == 0 {
		return fmt.Errorf("%w: fill model produced no legs for order %s",
			domain.ErrInvalidArgument, order.ClientOrderID)
	}
	venueOrderID := fmt.Sprintf("V-%d", e.seq.NextSeq())

	var legSum domain.Qty
	for i, leg := range legs {
		if !leg.Price.IsPositive() {
			return fmt.Errorf("%w: order %s leg %d priced non-positive %s at %s",
				domain.ErrInvalidArgument, order.ClientOrderID, i, leg.Price, bar.Symbol)
		}
		if leg.Qty <= 0 {
			return fmt.Errorf("%w: order %s leg %d non-positive qty %d",
				domain.ErrInvalidArgument, order.ClientOrderID, i, leg.Qty)
		}
		legSum += leg.Qty
		commission, cerr := e.model.Commission(leg.Qty, leg.Price)
		if cerr != nil {
			return fmt.Errorf("commission for order %s leg %d: %w", order.ClientOrderID, i, cerr)
		}
		tradeID := fmt.Sprintf("%s-%d-%d", venueOrderID, bar.TS.UnixNano(), i)
		f := domain.Fill{
			TradeID:       tradeID,
			ClientOrderID: order.ClientOrderID,
			VenueOrderID:  venueOrderID,
			StrategyID:    order.StrategyID,
			Symbol:        order.Symbol,
			Side:          order.Side,
			Qty:           leg.Qty,
			Price:         leg.Price,
			Commission:    commission,
			TS:            bar.TS,
		}
		if verr := f.Validate(); verr != nil {
			return fmt.Errorf("built invalid fill for order %s leg %d: %w", order.ClientOrderID, i, verr)
		}
		if serr := e.sink.EmitFill(f); serr != nil {
			return serr
		}
	}
	if legSum != order.Qty {
		return fmt.Errorf("%w: order %s legs sum to %d, want %d",
			domain.ErrInvalidArgument, order.ClientOrderID, legSum, order.Qty)
	}
	return nil
}

// PendingCount returns the number of orders awaiting a fill (next-bar models);
// the engine asserts this is 0 at end-of-run unless a final flush is expected.
func (e *SimExecutor) PendingCount() int {
	n := 0
	for _, os := range e.pending {
		n += len(os)
	}
	n += len(e.submittedThisBar)
	return n
}
