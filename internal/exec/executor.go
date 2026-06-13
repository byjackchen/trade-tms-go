package exec

// executor.go is the SimExecutor: it accepts order submissions from strategies
// during bar processing, simulates fills via the configured FillModel, and
// emits Fill events. It maintains the L1 "book" implicitly through the bar it
// processes (the nautilus-compat model reads the bar close; the realistic model
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

	// submittedThisBar holds orders submitted during the current bar's strategy
	// callback, for this-bar models, awaiting ProcessBar of the same bar.
	submittedThisBar map[string][]domain.Order
}

// NewSimExecutor wires an executor to a fill model, a fill sink and a
// deterministic sequence source.
func NewSimExecutor(model FillModel, sink FillSink, seq SeqSource) *SimExecutor {
	return &SimExecutor{
		model:            model,
		sink:             sink,
		seq:              seq,
		pending:          make(map[string][]domain.Order),
		submittedThisBar: make(map[string][]domain.Order),
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
		e.submittedThisBar[order.Symbol] = append(e.submittedThisBar[order.Symbol], order)
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
//   - this-bar model: the engine calls the strategy on_bar (which Submits),
//     THEN calls ProcessBar(bar); orders submitted on this bar fill against
//     this bar's close.
//   - next-bar model: the engine calls ProcessBar(bar) FIRST (filling orders
//     pending from prior bars against this bar's open), THEN the strategy
//     on_bar (which Submits orders pending for the next bar).
//
// Fills are emitted via the sink in deterministic order (orders in submission
// order for the symbol). Returns the number of fills emitted.
func (e *SimExecutor) ProcessBar(bar domain.Bar) (int, error) {
	var orders []domain.Order
	switch e.model.Timing() {
	case FillThisBar:
		orders = e.submittedThisBar[bar.Symbol]
		delete(e.submittedThisBar, bar.Symbol)
	case FillNextBar:
		orders = e.pending[bar.Symbol]
		delete(e.pending, bar.Symbol)
	}
	count := 0
	for _, order := range orders {
		if err := e.fill(order, bar); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

// fill prices the order against bar (possibly across several price legs) and
// emits one domain.Fill per leg, in leg order. Each leg gets the same venue
// order id (one order) and a distinct trade id (leg index appended), mirroring
// the reference's per-execution trade ids.
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
	for _, os := range e.submittedThisBar {
		n += len(os)
	}
	return n
}
