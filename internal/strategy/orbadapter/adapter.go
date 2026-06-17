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
	_ engine.SymbolScoped    = (*Strategy)(nil)
)

// ID returns the engine strategy id.
func (s *Strategy) ID() string { return s.id }

// SymbolsScoped declares the single symbol this adapter reacts to (engine
// symbol-indexed dispatch). The ORB generator self-filters bar.Symbol !=
// cfg.Symbol to a no-op, so the engine need only dispatch this name's bars here.
func (s *Strategy) SymbolsScoped() []string { return []string{s.gen.Symbol()} }

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
			if _, err := engine.CloseToFlat(sub, s.id, sig.Symbol, sig.TS, func(net domain.Qty) string {
				return fmt.Sprintf("[IntradayBreakout] FLAT (close %d) %s :: %s", net, sig.Symbol, sig.Reason)
			}); err != nil {
				return err
			}
		case orb.SideShort:
			// ORB never emits SHORT; ignore defensively.
			continue
		}
	}
	return nil
}

// EvaluateIntentJSON returns the single ORB intent for asOf, already bridged to
// the canonical domain.IntradayBreakoutIntent wire shape (engine.IntentEvaluator).
// It increments the SG's generation counter, as the publish path does.
//
// The adapter is the SANCTIONED domain bridge (modularization-review.md §E3): the
// local→domain normalization (formerly publish.normalizeORB) lives in
// NormalizeIntent here, so publish switches only on domain intent types and no
// longer imports strategy/orb. The pure orb.SignalIntent never escapes the adapter.
func (s *Strategy) EvaluateIntentJSON(asOf time.Time) any {
	return NormalizeIntent(s.gen.EvaluateIntent(asOf))
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
// orb.Bar whose OHLC reproduce the runner's Decimal(str(price)) translation. The
// engine renders prices at a 2-dp precision, so the 2-dp string is the exact
// value Decimal(str(x)) consumes — preserving the scale that propagates into the
// ORB reason / state strings.
func toORBBar(bar domain.Bar) orb.Bar {
	ob, _ := orb.NewBarFromStrings(
		bar.Symbol, bar.TS,
		bar.Open.StringFixed(2), bar.High.StringFixed(2),
		bar.Low.StringFixed(2), bar.Close.StringFixed(2),
		bar.Volume,
	)
	return ob
}
