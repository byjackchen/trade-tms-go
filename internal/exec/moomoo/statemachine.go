package moomoo

// statemachine.go is the order lifecycle state machine that drives domain
// order-state + fill events from moomoo Trd_UpdateOrder pushes. It is PURE and
// DETERMINISTIC (no IO, no clock, no locks): given the per-order prior state and
// one OrderUpdate it returns the resulting state + the Effects to apply (in
// order). The executor owns the locking, the clock and the side effects; keeping
// the transition pure makes the hard parts — idempotency, partial-fill delta
// accumulation, terminal handling — exhaustively unit-testable without a broker.
//
// FAITHFULNESS (locked decision 3): the status->effect mapping reproduces the
// Python adapter (src/adapters/moomoo/exec_client.py):
//   - moomoo dealtQty / dealtAvgPrice are CUMULATIVE; we hold the prior
//     cumulative snapshot and emit the per-fill DELTA (delta_qty, last_px =
//     delta_notional / delta_qty);
//   - a duplicate push (same or lower cumulative qty) yields NO fill effect
//     (idempotent — the engine never double-counts a re-emitted fill);
//   - terminal states (FILLED_ALL / canceled / rejected) are sticky: once an
//     order is terminal, later pushes are dropped as no-ops.

import (
	"fmt"
	"time"

	mo "github.com/byjackchen/trade-tms-go/internal/broker/moomoo"
	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// OrderState is the executor's per-order tracked state and the single source of
// truth for idempotency: the cumulative fill snapshot (CumQty / CumNotional)
// gates partial-fill deltas, and Status gates terminal stickiness.
type OrderState struct {
	ClientOrderID string
	VenueOrderID  string
	StrategyID    string
	Symbol        string
	Side          domain.OrderSide
	OrderQty      domain.Qty

	// Status is the domain lifecycle state derived so far (SUBMITTED on submit,
	// then ACCEPTED / PARTIALLY_FILLED / FILLED / CANCELED / REJECTED).
	Status domain.OrderStatus

	// CumQty / CumNotional are the cumulative filled qty and notional already
	// applied to accounting (CumNotional in 1e-4 money units). A fill push is
	// applied only for qty BEYOND CumQty; duplicates are no-ops.
	CumQty      domain.Qty
	CumNotional int64

	// fillSeq counts distinct fill effects emitted, so each gets a stable unique
	// trade id even when the venue supplies no fillID.
	fillSeq int
}

// IsTerminal reports whether the order has reached a terminal domain status.
func (s *OrderState) IsTerminal() bool { return s.Status.IsTerminal() }

// EffectKind enumerates the side effects a transition can request.
type EffectKind int

const (
	// EffectAccepted: the venue accepted the order (domain ACCEPTED). Persist the
	// order row; no accounting change.
	EffectAccepted EffectKind = iota
	// EffectFill: a per-execution fill delta to settle in accounting + persist as
	// a live.fills row + feed the engine. Carries a domain.Fill.
	EffectFill
	// EffectStatus: a status transition (PARTIALLY_FILLED / FILLED / CANCELED /
	// REJECTED) to persist on the order row + surface to the engine. No
	// accounting change beyond any accompanying EffectFill.
	EffectStatus
)

// Effect is one side effect a transition emits, in apply order. A single
// OrderUpdate can yield (at most) a fill delta followed by a status change (e.g.
// FILLED_ALL => EffectFill(last delta) then EffectStatus(FILLED)).
type Effect struct {
	Kind   EffectKind
	Status domain.OrderStatus // for EffectAccepted / EffectStatus
	Fill   domain.Fill        // for EffectFill (zero otherwise)
	// FillReversed marks an EffectStatus(CANCELED) produced by a FILL_CANCELLED
	// broker rollback: the caller logs at ERROR and relies on reconciliation, as
	// the already-applied fill is NOT auto-reversed (faithful to Python).
	FillReversed bool
	// Drift marks an unrecognised/UNSUBMITTED status: no state change, caller
	// logs a WARN. Kind is EffectStatus with an empty Status.
	Drift bool
	// DriftStatus carries the wire status name for a Drift effect's log line.
	DriftStatus string
}

// TradeIDFn produces the trade id for a fill effect. The executor supplies a
// deterministic generator (venue order id + sequence) so reruns reproduce ids;
// the state machine stays pure.
type TradeIDFn func(state *OrderState, fillIndex int) string

// Apply folds one OrderUpdate into state, returning the effects to apply in
// order. It mutates state in place (cumulative snapshot, status, fill seq).
// upd.ClientOrderID / VenueOrderID / Symbol must already be reconciled to this
// order by the caller. wallTS is the executor's clock-sourced fallback time for
// a fill that carries no venue timestamp.
//
// Idempotency + terminal stickiness:
//   - if state is already terminal, every further push is a no-op (nil effects);
//   - a fill push whose cumulative qty does not exceed state.CumQty yields no
//     fill effect (duplicate / re-emission).
func Apply(state *OrderState, upd mo.OrderUpdate, tradeID TradeIDFn, wallTS time.Time) ([]Effect, error) {
	if state.IsTerminal() {
		// Terminal orders never transition again. A late/duplicate push (e.g. a
		// repeated FILLED_ALL after we finalised) is silently ignored — the
		// idempotency guarantee at order granularity.
		return nil, nil
	}

	switch upd.Class() {
	case mo.StatusClassTransient:
		// SUBMITTING / WAITING_SUBMIT / CANCELLING_* — no domain event; the next
		// push delivers an accepted/terminal state.
		return nil, nil

	case mo.StatusClassAccepted:
		// SUBMITTED. Idempotent: re-accepting an already-accepted (or further-
		// advanced) order is a no-op.
		if state.Status == domain.OrderStatusAccepted ||
			state.Status == domain.OrderStatusPartiallyFilled {
			return nil, nil
		}
		state.Status = domain.OrderStatusAccepted
		return []Effect{{Kind: EffectAccepted, Status: domain.OrderStatusAccepted}}, nil

	case mo.StatusClassFilled:
		return applyFill(state, upd, tradeID, wallTS)

	case mo.StatusClassCanceled:
		state.Status = domain.OrderStatusCanceled
		return []Effect{{
			Kind:         EffectStatus,
			Status:       domain.OrderStatusCanceled,
			FillReversed: upd.IsFillCancelled(),
		}}, nil

	case mo.StatusClassRejected:
		state.Status = domain.OrderStatusRejected
		return []Effect{{Kind: EffectStatus, Status: domain.OrderStatusRejected}}, nil

	default: // StatusClassUnknown
		// UNSUBMITTED / unknown: no state change; caller logs a WARN.
		return []Effect{{Kind: EffectStatus, Drift: true, DriftStatus: upd.StatusName()}}, nil
	}
}

// applyFill converts a cumulative FILLED_PART/ALL push into the per-fill delta
// effect (if any) followed by the resulting status transition.
func applyFill(state *OrderState, upd mo.OrderUpdate, tradeID TradeIDFn, wallTS time.Time) ([]Effect, error) {
	var effects []Effect

	cumQty := upd.DealtQty
	cumAvg := upd.DealtAvgPrice
	if cumQty > 0 {
		// cumNotional = cumQty * cumAvgPrice (1e-4 money units). Guard overflow via
		// the domain Price helper.
		cumNotionalMoney, err := cumAvg.MulQty(cumQty)
		if err != nil {
			return nil, fmt.Errorf("order %s: cumulative notional: %w", state.ClientOrderID, err)
		}
		cumNotional := cumNotionalMoney.Raw()

		deltaQty := cumQty - state.CumQty
		deltaNotional := cumNotional - state.CumNotional
		if deltaQty > 0 && deltaNotional > 0 {
			// Per-fill price = delta_notional / delta_qty on the 1e-4 grid (matches
			// the Python adapter's f"{last_px:.4f}").
			lastPx := domain.Price(deltaNotional / int64(deltaQty))
			if !lastPx.IsPositive() {
				return nil, fmt.Errorf("order %s: non-positive per-fill price (deltaNotional=%d deltaQty=%d)",
					state.ClientOrderID, deltaNotional, deltaQty)
			}
			f := domain.Fill{
				TradeID:       tradeID(state, state.fillSeq),
				ClientOrderID: state.ClientOrderID,
				VenueOrderID:  state.VenueOrderID,
				StrategyID:    state.StrategyID,
				Symbol:        state.Symbol,
				Side:          state.Side,
				Qty:           deltaQty,
				Price:         lastPx,
				Commission:    0, // moomoo commission parsing is a future enhancement (matches Python TODO)
				TS:            fillTime(wallTS, upd.UpdateTimeNs),
			}
			if verr := f.Validate(); verr != nil {
				return nil, fmt.Errorf("order %s: built invalid fill: %w", state.ClientOrderID, verr)
			}
			// Advance the cumulative snapshot ATOMICALLY with emitting the effect so
			// a duplicate of THIS push is a no-op next time.
			state.CumQty = cumQty
			state.CumNotional = cumNotional
			state.fillSeq++
			effects = append(effects, Effect{Kind: EffectFill, Fill: f})
		}
		// deltaQty <= 0 (or notional regression): duplicate / re-emission — no fill.
	}

	// Status transition after the (possible) fill delta.
	newStatus := domain.OrderStatusPartiallyFilled
	if upd.IsFullFill() {
		newStatus = domain.OrderStatusFilled
	}
	if state.Status != newStatus {
		state.Status = newStatus
		effects = append(effects, Effect{Kind: EffectStatus, Status: newStatus})
	}
	return effects, nil
}

// fillTime picks the venue update time (UTC) when the push carries one, else the
// wall-clock time the executor supplied. The pure layer never calls time.Now.
func fillTime(wallTS time.Time, venueNs int64) time.Time {
	if venueNs > 0 {
		return time.Unix(0, venueNs).UTC()
	}
	return wallTS.UTC()
}
