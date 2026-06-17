package sectoradapter

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/engine"
	"github.com/byjackchen/trade-tms-go/internal/strategy/sectorrotation"
)

// Strategy adapts a sectorrotation.SignalGenerator to engine.Strategy plus the
// telemetry/persistence capability seams. One instance drives the whole
// universe (the SG is inherently multi-symbol).
type Strategy struct {
	id string
	sg *sectorrotation.SignalGenerator
}

// New wraps a constructed SignalGenerator under the engine strategy id (e.g.
// "SectorRotationRunner-000").
func New(id string, sg *sectorrotation.SignalGenerator) (*Strategy, error) {
	if id == "" {
		return nil, fmt.Errorf("%w: sector rotation adapter needs a non-empty id", domain.ErrInvalidArgument)
	}
	if sg == nil {
		return nil, fmt.Errorf("%w: sector rotation adapter needs a signal generator", domain.ErrInvalidArgument)
	}
	return &Strategy{id: id, sg: sg}, nil
}

// Compile-time capability assertions.
var (
	_ engine.Strategy            = (*Strategy)(nil)
	_ engine.SignalEvaluator     = (*Strategy)(nil)
	_ engine.StateSummarizer     = (*Strategy)(nil)
	_ engine.StatePersister      = (*Strategy)(nil)
	_ engine.SymbolScoped        = (*Strategy)(nil)
	_ engine.BatchWarmupConsumer = (*Strategy)(nil)
)

// ID returns the engine strategy id.
func (s *Strategy) ID() string { return s.id }

// SymbolsScoped declares the whole rotation universe (engine symbol-indexed
// dispatch). The generator returns nil + mutates NO state for any out-of-universe
// bar (signal.go: bar.Symbol not in universeSet), so scoping to exactly the
// universe is behaviour-preserving. The slice is the generator's universe in
// config order (read-only).
func (s *Strategy) SymbolsScoped() []string { return s.sg.Config().Universe }

// Generator exposes the underlying SignalGenerator (read-only use, e.g. tests).
func (s *Strategy) Generator() *sectorrotation.SignalGenerator { return s.sg }

// OnBar feeds the bar to the generator and translates the emitted signals into
// market orders, in the SG's emitted order (FLATs first, then LONGs; each group
// sorted by symbol — the rebalance ordering).
func (s *Strategy) OnBar(sub engine.OrderSubmitter, bar domain.Bar) error {
	for _, sig := range s.sg.OnBar(bar) {
		switch sig.Side {
		case domain.SideLong:
			if sig.TargetQty <= 0 {
				continue
			}
			reason := fmt.Sprintf("[SectorRot] LONG %d %s :: %s", sig.TargetQty, sig.Symbol, sig.Reason)
			if _, _, err := sub.SubmitMarketSignal(s.id, sig.Symbol, domain.SideLong, domain.OrderSideBuy, sig.TargetQty, reason, sig.TS); err != nil {
				return err
			}
		case domain.SideFlat:
			if _, err := engine.CloseToFlat(sub, s.id, sig.Symbol, sig.TS, func(net domain.Qty) string {
				return fmt.Sprintf("[SectorRot] FLAT (close %d) %s :: %s", net, sig.Symbol, sig.Reason)
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

// WarmupBatch primes the generator from the interleaved pre-window bar stream
// (engine.BatchWarmupConsumer): every bar is replayed through the SAME
// SignalGenerator.OnBar the run loop uses, building the rolling momentum history
// + currentPositions + month-rollover state EXACTLY as an in-loop backtest over
// those bars would — but the emitted rebalance signals are DISCARDED, so no order
// is ever submitted during priming (pure state build). The generator's state
// machine is self-contained (it updates currentPositions internally, not via the
// executor), so the post-warmup state is identical to a backtest that processed
// the same pre-window bars. Out-of-universe bars are ignored by the generator.
//
// LOOK-AHEAD SAFETY is the caller's contract: bars must be strictly before the
// run-window start (the assembler loads only [start-lookback, start) history).
func (s *Strategy) WarmupBatch(bars []domain.Bar) {
	for _, b := range bars {
		_ = s.sg.OnBar(b)
	}
}

// EvaluateSignalJSON returns the per-ETF SectorRotationSignal slice for asOf as
// a JSON-serializable value (engine.SignalEvaluator). Pure read of state.
func (s *Strategy) EvaluateSignalJSON(asOf time.Time) any {
	return s.sg.EvaluateSignal(asOf)
}

// StateSummaryJSON returns the light per-bar UI summary (engine.StateSummarizer).
func (s *Strategy) StateSummaryJSON() any {
	return s.sg.StateSummary()
}

// StateDictJSON returns the crash-recovery snapshot (engine.StatePersister).
func (s *Strategy) StateDictJSON() any {
	return s.sg.StateDict()
}

// LoadStateJSON restores the generator from a snapshot (engine.StatePersister).
func (s *Strategy) LoadStateJSON(b []byte) error {
	var d sectorrotation.StateDict
	if err := json.Unmarshal(b, &d); err != nil {
		return fmt.Errorf("sectoradapter: load state: %w", err)
	}
	return s.sg.LoadState(d)
}
