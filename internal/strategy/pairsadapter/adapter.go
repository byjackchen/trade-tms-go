package pairsadapter

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/engine"
	"github.com/byjackchen/trade-tms-go/internal/strategy/pairs"
)

// Strategy adapts a pairs.Generator to engine.Strategy plus the
// telemetry/persistence capability seams. One instance drives the whole pair
// universe (the SG is inherently multi-pair / multi-leg).
type Strategy struct {
	id string
	sg *pairs.Generator
}

// New wraps a constructed Generator under the engine strategy id (e.g.
// "Pairs-002").
func New(id string, sg *pairs.Generator) (*Strategy, error) {
	if id == "" {
		return nil, fmt.Errorf("%w: pairs adapter needs a non-empty id", domain.ErrInvalidArgument)
	}
	if sg == nil {
		return nil, fmt.Errorf("%w: pairs adapter needs a signal generator", domain.ErrInvalidArgument)
	}
	return &Strategy{id: id, sg: sg}, nil
}

// Compile-time capability assertions.
var (
	_ engine.Strategy            = (*Strategy)(nil)
	_ engine.IntentEvaluator     = (*Strategy)(nil)
	_ engine.StateSummarizer     = (*Strategy)(nil)
	_ engine.StatePersister      = (*Strategy)(nil)
	_ engine.SymbolScoped        = (*Strategy)(nil)
	_ engine.BatchWarmupConsumer = (*Strategy)(nil)
)

// ID returns the engine strategy id.
func (s *Strategy) ID() string { return s.id }

// SymbolsScoped declares EVERY leg symbol across all pairs (engine symbol-indexed
// dispatch). The pairs generator returns nil + mutates NO state for any bar whose
// symbol is not a configured leg (generator.go: bar.Symbol not in g.history), so
// scoping to exactly the leg universe is behaviour-preserving. Duplicate legs
// (a symbol appearing in two pairs) are harmless — the engine dedups — but we
// emit the full leg list (long then short, in pair-config order) for clarity.
func (s *Strategy) SymbolsScoped() []string {
	pairsCfg := s.sg.Config().Pairs
	syms := make([]string, 0, len(pairsCfg)*2)
	for _, p := range pairsCfg {
		syms = append(syms, p.LongLeg, p.ShortLeg)
	}
	return syms
}

// Generator exposes the underlying Generator (read-only use, e.g. tests).
func (s *Strategy) Generator() *pairs.Generator { return s.sg }

// OnBar feeds the bar to the generator and submits one order per emitted signal
// in the SG's emitted order (per pair: long_leg then short_leg; closes emit one
// FLAT per non-zero leg). The bar's canonical close string is derived from the
// fixed-point Price via the SG's Python-decimal bridge (exact for the <=4dp
// price domain).
func (s *Strategy) OnBar(sub engine.OrderSubmitter, bar domain.Bar) error {
	for _, sig := range s.sg.OnDomainBar(bar) {
		switch sig.Side {
		case domain.SideLong, domain.SideShort:
			if sig.TargetQty <= 0 {
				continue
			}
			side, err := domain.OrderSideFor(sig.Side)
			if err != nil {
				return err
			}
			if _, _, err := sub.SubmitMarketSignal(s.id, sig.Symbol, sig.Side, side, sig.TargetQty, sig.Reason, sig.TS); err != nil {
				return err
			}
		case domain.SideFlat:
			if _, err := engine.CloseToFlat(sub, s.id, sig.Symbol, sig.TS, func(net domain.Qty) string {
				return fmt.Sprintf("%s :: FLAT (close %d)", sig.Reason, net)
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

// WarmupBatch primes the generator from the interleaved pre-window bar stream
// (engine.BatchWarmupConsumer): every bar is replayed through the SAME
// Generator.OnDomainBar the run loop uses, building each leg's rolling close ring
// + lastBarDate sync state + per-pair state machine (z-score / beta / open
// positions) EXACTLY as an in-loop backtest over those bars would — but the
// emitted entry/exit signals are DISCARDED, so no order is ever submitted during
// priming (pure state build). The generator's pairState/positions are updated
// internally (not via the executor), so the post-warmup state is identical to a
// backtest that processed the same pre-window bars. Non-leg bars are ignored.
//
// LOOK-AHEAD SAFETY is the caller's contract: bars must be strictly before the
// run-window start (the assembler loads only [start-lookback, start) history).
func (s *Strategy) WarmupBatch(bars []domain.Bar) {
	for _, b := range bars {
		_ = s.sg.OnDomainBar(b)
	}
}

// EvaluateIntentJSON returns the 2N per-leg PairsSignalIntent slice for asOf as
// a JSON-serializable value (engine.IntentEvaluator). Pure read of state.
func (s *Strategy) EvaluateIntentJSON(asOf time.Time) any {
	return s.sg.EvaluateIntent(asOf)
}

// StateSummaryJSON returns the per-pair UI summary (engine.StateSummarizer).
func (s *Strategy) StateSummaryJSON() any {
	return s.sg.StateSummary()
}

// StateDictJSON returns the crash-recovery snapshot (engine.StatePersister).
func (s *Strategy) StateDictJSON() any {
	return s.sg.StateDict()
}

// LoadStateJSON restores the generator from a snapshot (engine.StatePersister).
func (s *Strategy) LoadStateJSON(b []byte) error {
	var d pairs.StateDict
	if err := json.Unmarshal(b, &d); err != nil {
		return fmt.Errorf("pairsadapter: load state: %w", err)
	}
	return s.sg.LoadState(d)
}
