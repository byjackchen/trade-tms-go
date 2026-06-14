// Package sepaadapter bridges the PURE SEPA SignalGenerator
// (internal/strategy/sepa) to the engine-facing Strategy seam
// (internal/engine), translating domain.Bar -> sepa.Bar, injecting per-bar
// portfolio context (regime / market-cap / earnings) via the engine's
// ContextConsumer seam, and translating emitted sepa.Signal target-position
// signals into market orders exactly as the reference NautilusRunner's
// _submit_for_signal (spec §10): LONG -> BUY target_qty; FLAT -> reverse the
// live net position to flat (no-op when already flat); SHORT unsupported.
//
// This package — NOT the pure sepa package — is the only place that imports
// engine/domain, preserving the [MUST-MATCH] layering constraint (spec §0):
// the core SEPA package never imports broker/engine code.
package sepaadapter

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/engine"
	"github.com/byjackchen/trade-tms-go/internal/strategy/sepa"
)

// Strategy adapts a sepa.Generator to engine.Strategy plus the P3 capability
// interfaces (ContextConsumer, IntentEvaluator, StateSummarizer,
// StatePersister). One instance drives one symbol.
type Strategy struct {
	id  string
	gen *sepa.Generator
	sym string
}

// New wraps a constructed sepa.Generator under the engine strategy id.
func New(id string, gen *sepa.Generator) *Strategy {
	return &Strategy{id: id, gen: gen, sym: gen.Symbol()}
}

// Compile-time capability assertions.
var (
	_ engine.Strategy        = (*Strategy)(nil)
	_ engine.ContextConsumer = (*Strategy)(nil)
	_ engine.WarmupConsumer  = (*Strategy)(nil)
	_ engine.IntentEvaluator = (*Strategy)(nil)
	_ engine.StateSummarizer = (*Strategy)(nil)
	_ engine.StatePersister  = (*Strategy)(nil)
)

// ID returns the engine strategy id.
func (s *Strategy) ID() string { return s.id }

// Generator returns the underlying pure SEPA generator (inspection/testing).
func (s *Strategy) Generator() *sepa.Generator { return s.gen }

// InjectContext pushes the per-bar context snapshot into the generator's
// setters before OnBar, mirroring the reference Actors' set_regime /
// set_market_cap / set_earnings_blackout pushes (spec §9.4). A missing
// per-ticker entry leaves the prior value (the reference's setters are only
// called on transitions; absent keys keep the last pushed value), so we only
// overwrite when the map carries this symbol.
func (s *Strategy) InjectContext(ctx engine.StrategyContext) {
	if ctx.Regime != "" {
		s.gen.SetRegime(ctx.Regime)
	}
	if ctx.MarketCapUSD != nil {
		if v, ok := ctx.MarketCapUSD[s.sym]; ok {
			s.gen.SetMarketCap(v)
		}
	}
	if ctx.EarningsBlackout != nil {
		if v, ok := ctx.EarningsBlackout[s.sym]; ok {
			s.gen.SetEarningsBlackout(v)
		}
	}
}

// WarmupBars primes the underlying SEPA generator's kline history from the
// pre-window bars for THIS adapter's symbol, mirroring
// SEPAUniverseRunner.warmup_ticker -> SignalGenerator.warmup_from_history
// (universe_runner.py:140-155, signal.py:378-392): pure state-priming, no signal
// evaluation, no orders. Offered every warmup symbol by the engine; only the
// matching symbol is consumed (one adapter drives one symbol). An empty history
// is a no-op.
func (s *Strategy) WarmupBars(sym string, history []domain.Bar) {
	if sym != s.sym || len(history) == 0 {
		return
	}
	bars := make([]sepa.Bar, len(history))
	for i, b := range history {
		bars[i] = toSepaBar(b)
	}
	s.gen.WarmupFromHistory(bars)
}

// OnBar feeds the bar to the generator and translates the emitted signals into
// market orders via the submitter (spec §10 _submit_for_signal).
func (s *Strategy) OnBar(sub engine.OrderSubmitter, bar domain.Bar) error {
	sigs := s.gen.OnBar(toSepaBar(bar))
	for _, sig := range sigs {
		if err := s.submit(sub, sig, bar.TS); err != nil {
			return err
		}
	}
	return nil
}

func (s *Strategy) submit(sub engine.OrderSubmitter, sig sepa.Signal, ts time.Time) error {
	switch sig.Side {
	case sepa.SideLong:
		if sig.TargetQty <= 0 {
			return nil
		}
		// LONG entry: gated by the portfolio (allocator budget + risk rules).
		_, _, err := sub.SubmitMarketSignal(s.id, sig.Symbol, domain.SideLong, domain.OrderSideBuy,
			domain.Qty(int64(sig.TargetQty)), sig.Reason, ts)
		return err
	case sepa.SideFlat:
		// Reverse the strategy's live net position to flat. The reference reads
		// the venue net position and submits a closing market order; a flat
		// book is a no-op. We delegate close-sizing to the engine by reading
		// the net through the submitter's position reader when available.
		net := s.netPosition(sub, sig.Symbol)
		side, ok := domain.CloseSideFor(net)
		if !ok {
			return nil // already flat
		}
		qty := net
		if qty < 0 {
			qty = -qty
		}
		reason := fmt.Sprintf("SEPA FLAT (close %d) %s", net, sig.Symbol)
		if _, err := sub.SubmitMarket(s.id, sig.Symbol, side, qty, reason, ts); err != nil {
			return err
		}
		return nil
	default:
		// SHORT unsupported (long-only SEPA); reference logs + skips.
		return nil
	}
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

// EvaluateIntentJSON returns the per-symbol SignalIntent for asOf
// (engine.IntentEvaluator). Pure read.
//
// It returns the RAW sepa.SignalIntent generator value — exactly like the
// other three adapters (orb/pairs/sector) return their own generator types —
// so publish.NormalizeIntent's `case sepa.SignalIntent` converts it to the
// canonical domain.SEPASignalIntent wire shape. Returning a private adapter
// struct here (the old behaviour) had no NormalizeIntent case and aborted every
// SEPA/multi intent in the signal/paper/live/EOD modes with
// "unsupported intent type"; keeping the raw type is the single source of the
// SEPA wire shape and restores the five-modes-one-engine thesis for SEPA.
func (s *Strategy) EvaluateIntentJSON(asOf time.Time) any {
	return s.gen.EvaluateIntent(asOf)
}

// StateSummaryJSON returns the light per-bar UI summary (engine.StateSummarizer).
func (s *Strategy) StateSummaryJSON() any {
	return marshalSummary(s.gen.StateSummary())
}

// StateDictJSON returns the crash-recovery snapshot (engine.StatePersister).
func (s *Strategy) StateDictJSON() any {
	return s.gen.StateDict()
}

// LoadStateJSON restores the generator from a snapshot (engine.StatePersister).
func (s *Strategy) LoadStateJSON(b []byte) error {
	var d sepa.StateDict
	if err := json.Unmarshal(b, &d); err != nil {
		return fmt.Errorf("sepaadapter: load state: %w", err)
	}
	s.gen.LoadState(d)
	return nil
}

func toSepaBar(b domain.Bar) sepa.Bar {
	return sepa.Bar{
		Symbol: b.Symbol,
		TS:     b.TS,
		Open:   b.Open.Float64(),
		High:   b.High.Float64(),
		Low:    b.Low.Float64(),
		Close:  b.Close.Float64(),
		Volume: b.Volume,
	}
}
