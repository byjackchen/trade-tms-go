package moomoo

// executor_manual.go adds the MANUAL (operator-driven) order primitives to the
// MoomooExecutor: a caller-supplied-client-order-id submit (so the manual desk
// owns the idempotency key) that supports MARKET and LIMIT order types, plus a
// cancel-by-client-order-id. These flow through the SAME tracking map, state
// machine, persistence, accounting and fill sink as the strategy-driven path —
// the only difference is the strategy id (a MANUAL pseudo-strategy, set by the
// caller) and that the client-order-id is supplied rather than auto-generated.
//
// SAFETY: SubmitManual reuses assertEnvInvariants on EVERY submission, so a paper
// executor can never reach TrdEnvReal and a live order must carry the live env +
// live trader id — exactly as the strategy path. There is no manual path to a
// real order that bypasses the live-activation gate (the executor is only ever
// live-bound through New() with a real-env Account (Account.IsReal()), which
// already enforced phrase + real acc id + UnlockTrade + live trader id). The
// upstream risk gate + per-order confirmation
// live in the manualtrade controller, not here.

import (
	"context"
	"fmt"
	"time"

	mo "github.com/byjackchen/trade-tms-go/internal/broker/moomoo"
	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// ManualOrderSpec is one operator-driven order submission. ClientOrderID is the
// caller-owned idempotency key (the manual desk derives it deterministically so a
// retried request never double-submits). Type is MARKET or LIMIT; LimitPrice is
// required (>0) for LIMIT and ignored for MARKET.
type ManualOrderSpec struct {
	ClientOrderID string
	StrategyID    string
	Symbol        string
	Side          domain.OrderSide
	Qty           domain.Qty
	Type          domain.OrderType
	LimitPrice    domain.Price
	Reason        string
	TS            time.Time
}

// SubmitManual submits an operator-driven order with a caller-supplied
// client-order-id (the idempotency key). It returns submitted=true when the order
// reached the venue (or was a no-op re-submit of a known coid), false on a
// validation/safety rejection. The portfolio risk gate + per-order confirmation
// have ALREADY run upstream (the manualtrade controller); this is the execution
// primitive. Idempotency: re-submitting a known client-order-id returns the
// existing tracked order WITHOUT a second PlaceOrder (the tracking map + the
// client both dedupe on the coid).
func (e *MoomooExecutor) SubmitManual(ctx context.Context, spec ManualOrderSpec) (string, bool, error) {
	if spec.ClientOrderID == "" {
		return "", false, fmt.Errorf("%w: manual order requires a client_order_id", domain.ErrInvalidArgument)
	}
	if spec.Type == "" {
		spec.Type = domain.OrderTypeMarket
	}
	if spec.TS.IsZero() {
		spec.TS = e.clock.Now()
	}

	// Idempotency short-circuit: a known coid that already reached the venue is a
	// no-op re-submit (return the tracked order). A known coid still in SUBMITTED
	// with no venue id falls through to re-place (the client dedupes at the wire).
	e.mu.Lock()
	if st, ok := e.orders[spec.ClientOrderID]; ok && st.VenueOrderID != "" {
		e.mu.Unlock()
		return spec.ClientOrderID, true, nil
	}
	e.mu.Unlock()

	order := domain.Order{
		ClientOrderID: spec.ClientOrderID,
		StrategyID:    spec.StrategyID,
		Symbol:        spec.Symbol,
		Side:          spec.Side,
		Type:          spec.Type,
		TIF:           domain.TIFGTC,
		Qty:           spec.Qty,
		Status:        domain.OrderStatusSubmitted,
		Reason:        spec.Reason,
		TS:            spec.TS,
	}
	if spec.Type == domain.OrderTypeLimit {
		lp := spec.LimitPrice
		order.LimitPrice = &lp
	}
	if err := order.Validate(); err != nil {
		return spec.ClientOrderID, false, fmt.Errorf("moomoo executor manual submit: %w", err)
	}

	req := mo.PlaceOrderRequest{
		AccID:         e.accID,
		TrdEnv:        e.env,
		ClientOrderID: spec.ClientOrderID,
		Symbol:        spec.Symbol,
		Side:          spec.Side,
		Type:          spec.Type,
		TIF:           domain.TIFGTC,
		Qty:           spec.Qty,
		Price:         spec.LimitPrice,
	}
	// SAFETY (decision 8): assert the env/acc binding on EVERY manual submission —
	// identical tripwire to the strategy path. A paper executor can never reach
	// TrdEnvReal; a live order must carry the real env + live trader id.
	if err := e.assertEnvInvariants(req); err != nil {
		e.rejected.Add(1)
		e.recordRisk(ctx, spec.StrategyID, spec.Symbol, "safety.env_invariant", err.Error())
		return spec.ClientOrderID, false, err
	}

	// Register the tracked order up-front (idempotent: re-registering the same coid
	// keeps existing state so a retried submit never resets fills).
	e.mu.Lock()
	st, exists := e.orders[spec.ClientOrderID]
	if !exists {
		st = &OrderState{
			ClientOrderID: spec.ClientOrderID,
			StrategyID:    spec.StrategyID,
			Symbol:        spec.Symbol,
			Side:          spec.Side,
			OrderQty:      spec.Qty,
			Status:        domain.OrderStatusSubmitted,
		}
		e.orders[spec.ClientOrderID] = st
	}
	e.mu.Unlock()

	e.persistOrder(ctx, order)

	res, err := e.cfg.Client.PlaceOrder(ctx, req)
	if err != nil {
		e.mu.Lock()
		st.Status = domain.OrderStatusRejected
		e.mu.Unlock()
		rejected := order
		rejected.Status = domain.OrderStatusRejected
		rejected.Reason = err.Error()
		e.persistOrder(ctx, rejected)
		e.recordRisk(ctx, spec.StrategyID, spec.Symbol, "exec.place_failed", err.Error())
		return spec.ClientOrderID, false, fmt.Errorf("moomoo executor: manual place order %s: %w", spec.ClientOrderID, err)
	}

	e.mu.Lock()
	st.VenueOrderID = res.VenueOrderID
	if res.VenueOrderID != "" {
		e.venIndex[res.VenueOrderID] = spec.ClientOrderID
	}
	e.mu.Unlock()
	e.submitted.Add(1)
	return spec.ClientOrderID, true, nil
}

// CancelManual requests the venue cancel a working order by client-order-id. The
// terminal CANCELLED push (Trd_UpdateOrder) drives the state machine to CANCELED;
// this issues the request. Cancelling an unknown / already-terminal order is a
// no-op (the client + state machine both treat it idempotently). An ErrUnsupported
// from a client that cannot cancel (the wire build without the modify-order proto)
// is surfaced so the operator is never told "cancelled" on a working real order.
func (e *MoomooExecutor) CancelManual(ctx context.Context, clientOrderID string) error {
	if clientOrderID == "" {
		return fmt.Errorf("%w: cancel requires a client_order_id", domain.ErrInvalidArgument)
	}
	// A locally-known terminal order is already done — short-circuit (idempotent).
	e.mu.Lock()
	if st, ok := e.orders[clientOrderID]; ok && st.IsTerminal() {
		e.mu.Unlock()
		return nil
	}
	e.mu.Unlock()
	if err := e.cfg.Client.CancelOrder(ctx, e.accID, e.env, clientOrderID); err != nil {
		e.recordRisk(ctx, "", "", "exec.cancel_failed", err.Error())
		return fmt.Errorf("moomoo executor: cancel order %s: %w", clientOrderID, err)
	}
	return nil
}

// TrackedOrder returns a snapshot of the tracked order for clientOrderID (the
// manual desk reads it to report status); ok=false when untracked.
func (e *MoomooExecutor) TrackedOrder(clientOrderID string) (OrderState, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	st, ok := e.orders[clientOrderID]
	if !ok {
		return OrderState{}, false
	}
	return *st, true
}
