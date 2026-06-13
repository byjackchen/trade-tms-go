// Package sectoradapter bridges the PURE SectorRotation SignalGenerator
// (internal/strategy/sector_rotation) to the engine-facing Strategy seam
// (internal/engine), translating the SG's multi-symbol rebalance signals into
// market orders exactly as the reference SectorRotationRunner._submit_for_signal
// (strategy-sector-orb.md, nautilus_runner.py):
//
//   - LONG -> BUY of signal.target_qty (the SG only emits LONG for symbols not
//     currently held, so target_qty is the full target position).
//   - FLAT -> close the entire live net position (SELL if long, BUY if short);
//     a flat book is a no-op.
//
// This package — NOT the pure sector_rotation package — is the only place that
// imports engine, preserving the Eng-D2 two-layer constraint: the core
// strategy package never imports broker/engine code. It also implements the
// P3 capability seams (IntentEvaluator, StateSummarizer, StatePersister) the
// engine probes by type assertion. SectorRotation consumes no per-bar context,
// so ContextConsumer is intentionally NOT implemented.
package sectoradapter

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/engine"
	sr "github.com/byjackchen/trade-tms-go/internal/strategy/sector_rotation"
)

// Strategy adapts a sector_rotation.SignalGenerator to engine.Strategy plus the
// telemetry/persistence capability seams. One instance drives the whole
// universe (the SG is inherently multi-symbol).
type Strategy struct {
	id string
	sg *sr.SignalGenerator
}

// New wraps a constructed SignalGenerator under the engine strategy id (e.g.
// "SectorRotationRunner-000").
func New(id string, sg *sr.SignalGenerator) (*Strategy, error) {
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
func (s *Strategy) Generator() *sr.SignalGenerator { return s.sg }

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
			net := s.netPosition(sub, sig.Symbol)
			side, ok := domain.CloseSideFor(net)
			if !ok {
				continue // already flat
			}
			qty := net
			if qty < 0 {
				qty = -qty
			}
			reason := fmt.Sprintf("[SectorRot] FLAT (close %d) %s :: %s", net, sig.Symbol, sig.Reason)
			if _, err := sub.SubmitMarket(s.id, sig.Symbol, side, qty, reason, sig.TS); err != nil {
				return err
			}
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
	var d sr.StateDict
	if err := json.Unmarshal(b, &d); err != nil {
		return fmt.Errorf("sectoradapter: load state: %w", err)
	}
	return s.sg.LoadState(d)
}
