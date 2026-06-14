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
func New(id string, gen *sepa.Generator) (*Strategy, error) {
	if id == "" {
		return nil, fmt.Errorf("%w: sepa adapter needs a non-empty id", domain.ErrInvalidArgument)
	}
	if gen == nil {
		return nil, fmt.Errorf("%w: sepa adapter needs a signal generator", domain.ErrInvalidArgument)
	}
	return &Strategy{id: id, gen: gen, sym: gen.Symbol()}, nil
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
		// book is a no-op. Close-sizing is delegated to the shared engine helper
		// (reads the net through the submitter's PositionReader when available).
		_, err := engine.CloseToFlat(sub, s.id, sig.Symbol, ts, func(net domain.Qty) string {
			return fmt.Sprintf("SEPA FLAT (close %d) %s", net, sig.Symbol)
		})
		return err
	default:
		// SHORT unsupported (long-only SEPA); reference logs + skips.
		return nil
	}
}

// EvaluateIntentJSON returns the per-symbol SEPA intent for asOf, already
// bridged to the canonical domain.SEPASignalIntent wire shape
// (engine.IntentEvaluator). Pure read.
//
// The adapter is the SANCTIONED domain bridge (modularization-review.md §E3): it
// is the one place that imports both the pure sepa package (which must stay
// zero-domain for byte-for-byte golden parity) AND domain. The local→domain
// normalization (formerly publish.normalizeSEPA) now lives in NormalizeIntent
// here, so publish switches only on domain intent types and no longer imports
// strategy/sepa. The pure sepa.SignalIntent never escapes the adapter.
func (s *Strategy) EvaluateIntentJSON(asOf time.Time) any {
	return NormalizeIntent(s.gen.EvaluateIntent(asOf))
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
