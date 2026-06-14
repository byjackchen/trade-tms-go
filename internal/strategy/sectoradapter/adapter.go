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
	_ engine.Strategy        = (*Strategy)(nil)
	_ engine.IntentEvaluator = (*Strategy)(nil)
	_ engine.StateSummarizer = (*Strategy)(nil)
	_ engine.StatePersister  = (*Strategy)(nil)
)

// ID returns the engine strategy id.
func (s *Strategy) ID() string { return s.id }

// Generator exposes the underlying SignalGenerator (read-only use, e.g. tests).
func (s *Strategy) Generator() *sectorrotation.SignalGenerator { return s.sg }

// OnBar feeds the bar to the generator and translates the emitted signals into
// market orders, in the SG's emitted order (FLATs first, then LONGs; each group
// sorted by symbol — matching the reference rebalance ordering).
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

// EvaluateIntentJSON returns the per-ETF SectorRotationIntent slice for asOf as
// a JSON-serializable value (engine.IntentEvaluator). Pure read of state.
func (s *Strategy) EvaluateIntentJSON(asOf time.Time) any {
	return s.sg.EvaluateIntent(asOf)
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
