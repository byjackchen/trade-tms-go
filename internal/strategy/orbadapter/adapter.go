// Package orbadapter bridges the PURE ORB (intraday_breakout) SignalGenerator
// (internal/strategy/orb) to the engine-facing Strategy seam (internal/engine),
// translating the SG's signals into market orders exactly as the reference
// IntradayBreakoutRunner does (strategy-sector-orb.md §3.10, nautilus_runner.py):
//
//   - LONG -> BUY of signal.target_qty, TimeInForce DAY (intraday-only; the
//     engine's market submit is the day-scoped equivalent). The SG sizes the
//     full target position.
//   - FLAT -> close the entire LIVE net position (SELL if long, BUY if short),
//     read from portfolio.net_position; a flat book is a no-op. The SG's carried
//     FLAT qty is NOT used for sizing (it is only the SG's internal held count,
//     surfaced in logs).
//
// This package — NOT the pure orb package — is the only place that imports
// engine, preserving the Eng-D2 two-layer constraint: the core strategy package
// never imports broker/engine code (the AST test on the Python side asserts the
// same). It implements the P3 capability seams (IntentEvaluator,
// StateSummarizer, StatePersister). ORB consumes no per-bar portfolio context
// (no regime/market-cap/earnings — nautilus_runner.py:_runner_ticker reserves
// per-ticker routing but subscribes to none), so ContextConsumer is
// intentionally NOT implemented.
package orbadapter

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/engine"
	"github.com/byjackchen/trade-tms-go/internal/strategy/orb"
)

// Strategy adapts an orb.Generator to engine.Strategy plus the telemetry/
// persistence capability seams. One instance drives a single instrument (ORB is
// single-symbol).
type Strategy struct {
	id  string
	gen *orb.Generator
}

// New wraps a constructed orb.Generator under the engine strategy id (e.g.
// "IntradayBreakoutRunner-000").
func New(id string, gen *orb.Generator) (*Strategy, error) {
	if id == "" {
		return nil, fmt.Errorf("%w: orb adapter needs a non-empty id", domain.ErrInvalidArgument)
	}
	if gen == nil {
		return nil, fmt.Errorf("%w: orb adapter needs a signal generator", domain.ErrInvalidArgument)
	}
	return &Strategy{id: id, gen: gen}, nil
}

// Compile-time capability assertions.
var (
	_ engine.Strategy        = (*Strategy)(nil)
	_ engine.IntentEvaluator = (*Strategy)(nil)
	_ engine.StateSummarizer = (*Strategy)(nil)
	_ engine.StatePersister  = (*Strategy)(nil)
)

// ID returns the engine strategy id.
func (s *Strategy) ID() string { return s.id }

// Generator exposes the underlying generator (read-only use, e.g. tests).
func (s *Strategy) Generator() *orb.Generator { return s.gen }

// OnBar translates the bar to the pure orb.Bar, runs the generator, and submits
// the resulting orders in the SG's emitted order (a session-boundary defensive
// FLAT, when present, precedes the new session's entry — same ordering the SG
// returns).
func (s *Strategy) OnBar(sub engine.OrderSubmitter, bar domain.Bar) error {
	ob := toORBBar(bar)
	for _, sig := range s.gen.OnBar(ob) {
		switch sig.Side {
		case orb.SideLong:
			if sig.TargetQty <= 0 {
				continue
			}
			qty := domain.Qty(sig.TargetQty)
			reason := fmt.Sprintf("[IntradayBreakout] LONG %d %s :: %s", sig.TargetQty, sig.Symbol, sig.Reason)
			if _, _, err := sub.SubmitMarketSignal(s.id, sig.Symbol, domain.SideLong, domain.OrderSideBuy, qty, reason, sig.TS); err != nil {
				return err
			}
		case orb.SideFlat:
			net := s.netPosition(sub, sig.Symbol)
			side, ok := domain.CloseSideFor(net)
			if !ok {
				continue // already flat
			}
			qty := net
			if qty < 0 {
				qty = -qty
			}
			reason := fmt.Sprintf("[IntradayBreakout] FLAT (close %d) %s :: %s", net, sig.Symbol, sig.Reason)
			if _, err := sub.SubmitMarket(s.id, sig.Symbol, side, qty, reason, sig.TS); err != nil {
				return err
			}
		case orb.SideShort:
			// ORB never emits SHORT; ignore defensively.
			continue
		}
	}
	return nil
}

// netPosition reads the strategy's live net position if the submitter exposes a
// PositionReader; otherwise 0 (the FLAT path then no-ops, matching a book the
// engine has already flattened).
func (s *Strategy) netPosition(sub engine.OrderSubmitter, sym string) domain.Qty {
	if pr, ok := sub.(engine.PositionReader); ok {
		return pr.NetPosition(s.id, sym)
	}
	return 0
}

// EvaluateIntentJSON returns the single IntradayBreakoutIntent for asOf as a
// JSON-serializable value (engine.IntentEvaluator). It increments the SG's
// generation counter, exactly as the reference runner's publish path does.
func (s *Strategy) EvaluateIntentJSON(asOf time.Time) any {
	return s.gen.EvaluateIntent(asOf)
}

// StateSummaryJSON returns the light per-bar UI summary (engine.StateSummarizer).
func (s *Strategy) StateSummaryJSON() any {
	return s.gen.StateSummary()
}

// StateDictJSON returns the crash-recovery snapshot (engine.StatePersister).
func (s *Strategy) StateDictJSON() any {
	return s.gen.StateDict()
}

// LoadStateJSON restores the generator from a snapshot (engine.StatePersister).
func (s *Strategy) LoadStateJSON(b []byte) error {
	var d orb.StateDict
	if err := json.Unmarshal(b, &d); err != nil {
		return fmt.Errorf("orbadapter: load state: %w", err)
	}
	return s.gen.LoadState(d)
}

// toORBBar translates a domain.Bar (2-dp exact 1e-4 fixed point, UTC) to an
// orb.Bar whose OHLC reproduce the reference runner's Decimal(str(price))
// translation. The engine renders prices at Nautilus price_precision=2, so the
// 2-dp string is the exact value Python's Decimal(str(x)) consumes — preserving
// the scale that propagates into the ORB reason / state strings.
func toORBBar(bar domain.Bar) orb.Bar {
	ob, _ := orb.NewBarFromStrings(
		bar.Symbol, bar.TS,
		bar.Open.StringFixed(2), bar.High.StringFixed(2),
		bar.Low.StringFixed(2), bar.Close.StringFixed(2),
		bar.Volume,
	)
	return ob
}
