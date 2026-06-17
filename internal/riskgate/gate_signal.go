package riskgate

// gate_signal.go factors the pre-trade-gate WRAPPER that was reimplemented per
// execution mode (engine.orderSubmitter.SubmitMarketSignal and
// livetrade.GatedSubmitter.SubmitMarketSignal — modularization-review.md §E1).
// Both callers built a ProposedOrder, ran Gate.Check, and on a rejection
// recorded the decision + returned "not submitted". That wrapper now lives HERE
// so the gate semantics cannot drift between the backtest and live paths.
//
// What stays in the CALLER (mode-specific, deliberately NOT factored here):
//   - the FLAT / qty<=0 bypass (closes always proceed — done before GateSignal),
//   - the live-only daily-loss-halt latch (a pre-check that produces its OWN
//     rejection ahead of the portfolio gate; live wires it via its halt predicate),
//   - the actual submit-to-executor on approval, and
//   - building the snapshot + estimated price from the caller's account book.
//
// Each caller supplies only its own RejectionRecorder sink (the engine appends to
// its RejectedOrder slice; the live submitter persists a live.risk_events row).

import (
	"time"

	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// Rejection is one gate rejection handed to a RejectionRecorder. It carries the
// rejected order's identity, the rule + reason that rejected it, and the
// estimated price / timestamp — the union of the fields the two mode-specific
// sinks persist (the engine's RejectedOrder ignores Price; the live submitter's
// risk-events row keeps it).
type Rejection struct {
	StrategyID string
	Symbol     string
	Side       domain.SignalSide
	Qty        domain.Qty
	Price      domain.Price
	RuleName   string
	Reason     string
	TS         time.Time
}

// RejectionRecorder is the pluggable sink GateSignal calls for a rejected order.
// The engine appends to its in-memory RejectedOrder slice; the live submitter
// persists a live.risk_events row + audit. May be a no-op (a nil-equivalent sink
// that drops the rejection); GateSignal still reports the decision either way.
type RejectionRecorder interface {
	RecordRejection(r Rejection)
}

// GateSignal runs the portfolio gate for one strategy SIGNAL and, on a rejection,
// hands the decision to the recorder. It returns the RiskDecision so the caller
// can branch on Approved (submit vs skip). A nil gate ALWAYS approves (no gate
// configured => every order submits), matching both legacy callers.
//
// The caller MUST have already handled the FLAT / qty<=0 bypass and (live) the
// daily-loss-halt pre-check before calling GateSignal — this is only the
// allocator-budget + aggregate-risk wrapper, byte-identical to the old per-mode
// code. The estimated price is carried through to the recorder unchanged.
func GateSignal(gate *Gate, proposed ProposedOrder, account PortfolioSnapshot, price domain.Price, rec RejectionRecorder) RiskDecision {
	if gate == nil {
		return Approve()
	}
	decision := gate.Check(proposed, account)
	if !decision.Approved && rec != nil {
		rec.RecordRejection(Rejection{
			StrategyID: proposed.StrategyID,
			Symbol:     proposed.Symbol,
			Side:       proposed.Side,
			Qty:        domain.Qty(proposed.Qty),
			Price:      price,
			RuleName:   decision.RuleName,
			Reason:     decision.Reason,
			TS:         proposed.TS,
		})
	}
	return decision
}
