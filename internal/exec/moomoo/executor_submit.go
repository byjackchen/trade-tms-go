package moomoo

// executor_submit.go is the order-submission + push-handling half of the
// MoomooExecutor: the engine.OrderSubmitter / engine.PositionReader methods the
// strategies call, plus the Trd_UpdateOrder / Trd_UpdateOrderFill push handlers
// that drive the per-order state machine and fan its effects out to accounting,
// persistence and the engine fill sink.

import (
	"context"
	"fmt"
	"time"

	mo "github.com/byjackchen/trade-tms-go/internal/adapters/moomoo"
	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// SubmitMarket builds and submits a MARKET order for the strategy, returning the
// assigned client-order-id (the idempotency key). The portfolio gate runs
// UPSTREAM (in the session/runner) — this is the ungated primitive used for
// already-decided opening orders AND for FLAT/close orders. A submit failure is
// returned so the caller can record a risk event; the order is NOT tracked
// unless PlaceOrder returned (the client guarantees idempotent submission).
func (e *MoomooExecutor) SubmitMarket(strategyID, symbol string, side domain.OrderSide, qty domain.Qty, reason string, ts time.Time) (string, error) {
	coid := e.nextClientOrderID()
	if _, err := e.place(context.Background(), coid, strategyID, symbol, side, qty, reason, ts); err != nil {
		return coid, err
	}
	return coid, nil
}

// SubmitMarketSignal submits a market order for a strategy SIGNAL. The portfolio
// gate has ALREADY run upstream (decision 4 wires the gate as a pre-submit step
// in the session); by the time the strategy adapter calls this, the decision is
// made. signalSide is informational here (carried for parity with the engine
// seam). It always submits and reports submitted=true (a gate rejection upstream
// means this is never reached for the rejected order). A genuine PlaceOrder
// error is returned as err (NOT a gate rejection).
func (e *MoomooExecutor) SubmitMarketSignal(strategyID, symbol string, signalSide domain.SignalSide, orderSide domain.OrderSide, qty domain.Qty, reason string, ts time.Time) (string, bool, error) {
	coid := e.nextClientOrderID()
	if _, err := e.place(context.Background(), coid, strategyID, symbol, orderSide, qty, reason, ts); err != nil {
		return coid, false, err
	}
	_ = signalSide
	return coid, true, nil
}

// NetPosition returns the strategy's signed net position in symbol (0 if flat),
// read from the account book — the same source the SimExecutor/engine use, so a
// strategy's FLAT-close sizing behaves identically to backtest.
func (e *MoomooExecutor) NetPosition(strategyID, symbol string) domain.Qty {
	pos, ok := e.cfg.Account.Position(strategyID, symbol)
	if !ok {
		return 0
	}
	return pos.SignedQty
}

// place is the single submission path. It builds + validates the domain Order,
// asserts the live-safety invariants, registers the OrderState (so a push that
// races ahead of the PlaceOrder reply still correlates), then calls the client.
// Registering BEFORE the call is deliberate: the broker push can arrive before
// the synchronous reply, and the state machine must already have the order.
func (e *MoomooExecutor) place(ctx context.Context, coid, strategyID, symbol string, side domain.OrderSide, qty domain.Qty, reason string, ts time.Time) (mo.PlaceOrderResult, error) {
	order := domain.NewMarketOrder(coid, strategyID, symbol, side, qty, reason, ts)
	if err := order.Validate(); err != nil {
		return mo.PlaceOrderResult{}, fmt.Errorf("moomoo executor submit: %w", err)
	}

	req := mo.PlaceOrderRequest{
		AccID:         e.accID,
		TrdEnv:        e.env,
		ClientOrderID: coid,
		Symbol:        symbol,
		Side:          side,
		Type:          domain.OrderTypeMarket,
		TIF:           domain.TIFGTC,
		Qty:           qty,
	}
	// SAFETY (decision 8): assert the env/acc binding is internally consistent on
	// EVERY submission — a paper executor can never reach TrdEnvReal, and a live
	// order must carry the real env. This is defence-in-depth on top of the
	// constructor gate: no order leaves without passing here.
	if err := e.assertEnvInvariants(req); err != nil {
		e.rejected.Add(1)
		e.recordRisk(ctx, strategyID, symbol, "safety.env_invariant", err.Error())
		return mo.PlaceOrderResult{}, err
	}

	// Register the tracked order up-front (idempotent: re-registering the same
	// coid keeps the existing state, so a retried submit never resets fills).
	e.mu.Lock()
	st, exists := e.orders[coid]
	if !exists {
		st = &OrderState{
			ClientOrderID: coid,
			StrategyID:    strategyID,
			Symbol:        symbol,
			Side:          side,
			OrderQty:      qty,
			Status:        domain.OrderStatusSubmitted,
		}
		e.orders[coid] = st
	}
	e.mu.Unlock()

	// Persist the SUBMITTED order row (idempotent upsert) so a crash between
	// submit and the first push still leaves a durable record to reconcile.
	e.persistOrder(ctx, order)

	res, err := e.cfg.Client.PlaceOrder(ctx, req)
	if err != nil {
		// The submit failed at the venue. Mark the tracked order REJECTED so the
		// state machine treats it as terminal (no fills can arrive for an order the
		// venue never accepted), persist + surface.
		e.mu.Lock()
		st.Status = domain.OrderStatusRejected
		e.mu.Unlock()
		rejected := order
		rejected.Status = domain.OrderStatusRejected
		rejected.Reason = err.Error()
		e.persistOrder(ctx, rejected)
		e.recordRisk(ctx, strategyID, symbol, "exec.place_failed", err.Error())
		return mo.PlaceOrderResult{}, fmt.Errorf("moomoo executor: place order %s: %w", coid, err)
	}

	// Bind the venue id so pushes keyed by venue-order-id correlate back.
	e.mu.Lock()
	st.VenueOrderID = res.VenueOrderID
	if res.VenueOrderID != "" {
		e.venIndex[res.VenueOrderID] = coid
	}
	e.mu.Unlock()
	e.submitted.Add(1)
	return res, nil
}

// assertEnvInvariants is the compiled SAFETY assertion (decision 8): there is no
// path that submits a non-paper order without the live binding, and a paper
// binding can never carry TrdEnvReal.
func (e *MoomooExecutor) assertEnvInvariants(req mo.PlaceOrderRequest) error {
	if req.TrdEnv != e.env {
		return fmt.Errorf("%w: order env %s != executor env %s", domain.ErrInvalidArgument, req.TrdEnv, e.env)
	}
	if req.AccID != e.accID {
		return fmt.Errorf("%w: order acc_id %d != bound acc_id %d", domain.ErrInvalidArgument, req.AccID, e.accID)
	}
	if e.env == mo.TrdEnvReal {
		// A live order is only reachable through New(ModeLive), which already
		// verified phrase + real acc id + live trader id + UnlockTrade. Re-assert
		// the trader-id binding as a tripwire.
		if e.cfg.TraderID != LiveTraderID {
			return fmt.Errorf("%w: live submission without the %s trader-id", domain.ErrInvalidArgument, LiveTraderID)
		}
	}
	return nil
}

// onOrderUpdate is the Trd_UpdateOrder push handler (client reader goroutine).
// It correlates the push to a tracked order, steps the state machine, and
// applies the effects.
func (e *MoomooExecutor) onOrderUpdate(upd mo.OrderUpdate) {
	st := e.lookup(upd)
	if st == nil {
		// Unknown order (likely placed outside this process / external). Drop it —
		// the Python adapter does the same; reconciliation surfaces any drift.
		return
	}
	effects, err := Apply(st, upd, e.tradeID, e.clock.Now())
	if err != nil {
		e.logf("moomoo executor: apply order update for %s: %v", st.ClientOrderID, err)
		return
	}
	e.applyEffects(context.Background(), st, effects)
}

// onFillUpdate is the Trd_UpdateOrderFill push handler. Accounting is driven by
// the cumulative OrderUpdate deltas (the gap-free authoritative source), so the
// per-execution fill push is corroborating only: we verify it correlates to a
// tracked order and log a warning if it does not, without double-applying.
func (e *MoomooExecutor) onFillUpdate(fu mo.FillUpdate) {
	e.mu.Lock()
	coid := fu.ClientOrderID
	if coid == "" && fu.VenueOrderID != "" {
		coid = e.venIndex[fu.VenueOrderID]
	}
	_, known := e.orders[coid]
	e.mu.Unlock()
	if !known {
		e.logf("moomoo executor: fill push for unknown order (venue=%s) — reconciliation will catch", fu.VenueOrderID)
	}
	// No accounting mutation here by design (see doc above).
}

// lookup resolves the tracked OrderState for a push, by client-order-id (the
// remark) or, failing that, the venue-order-id index.
func (e *MoomooExecutor) lookup(upd mo.OrderUpdate) *OrderState {
	e.mu.Lock()
	defer e.mu.Unlock()
	if upd.ClientOrderID != "" {
		if st, ok := e.orders[upd.ClientOrderID]; ok {
			if st.VenueOrderID == "" && upd.VenueOrderID != "" {
				st.VenueOrderID = upd.VenueOrderID
				e.venIndex[upd.VenueOrderID] = upd.ClientOrderID
			}
			return st
		}
	}
	if upd.VenueOrderID != "" {
		if coid, ok := e.venIndex[upd.VenueOrderID]; ok {
			return e.orders[coid]
		}
	}
	return nil
}

// tradeID is the deterministic trade-id generator passed to the state machine:
// "<venueOrderID|clientOrderID>-<fillIndex>". The state machine guarantees a
// fresh fillIndex per emitted fill, so ids are unique + reproducible.
func (e *MoomooExecutor) tradeID(st *OrderState, fillIndex int) string {
	base := st.VenueOrderID
	if base == "" {
		base = st.ClientOrderID
	}
	return fmt.Sprintf("%s-%d", base, fillIndex)
}

// applyEffects fans the state-machine effects out, in order, to persistence,
// accounting and the engine fill sink. It is the ONLY place that mutates the
// account from a push, so partial-fill accumulation stays single-threaded under
// the (already released) state lock — effects are applied without holding mu so
// downstream IO cannot deadlock the reader goroutine against a Submit.
func (e *MoomooExecutor) applyEffects(ctx context.Context, st *OrderState, effects []Effect) {
	for _, eff := range effects {
		switch eff.Kind {
		case EffectAccepted:
			e.persistOrder(ctx, e.orderSnapshot(st, eff.Status))

		case EffectFill:
			// Settle the fill in accounting FIRST (authoritative position), then
			// persist the fill + the resulting position, then feed the engine.
			pos, err := e.cfg.Account.ApplyFill(eff.Fill)
			if err != nil {
				e.logf("moomoo executor: accounting apply fill %s: %v", eff.Fill.TradeID, err)
				continue
			}
			e.persistFill(ctx, eff.Fill)
			e.persistPosition(ctx, pos)
			if e.cfg.Sink != nil {
				if serr := e.cfg.Sink.EmitFill(eff.Fill); serr != nil {
					e.logf("moomoo executor: emit fill %s to engine: %v", eff.Fill.TradeID, serr)
				}
			}
			e.fillsEmit.Add(1)

		case EffectStatus:
			if eff.Drift {
				e.logf("moomoo executor: order %s drift status %q (no transition); reconciliation will catch",
					st.ClientOrderID, eff.DriftStatus)
				continue
			}
			if eff.FillReversed {
				e.logf("moomoo executor: order %s FILL_CANCELLED — broker rolled back a fill; "+
					"position cache diverges until reconciliation", st.ClientOrderID)
				e.recordRisk(ctx, st.StrategyID, st.Symbol, "exec.fill_cancelled",
					"broker rolled back a previously-reported fill")
			}
			e.persistOrder(ctx, e.orderSnapshot(st, eff.Status))
		}
	}
}

// orderSnapshot builds a domain.Order snapshot of the tracked state at status.
func (e *MoomooExecutor) orderSnapshot(st *OrderState, status domain.OrderStatus) domain.Order {
	return domain.Order{
		ClientOrderID: st.ClientOrderID,
		VenueOrderID:  st.VenueOrderID,
		StrategyID:    st.StrategyID,
		Symbol:        st.Symbol,
		Side:          st.Side,
		Type:          domain.OrderTypeMarket,
		TIF:           domain.TIFGTC,
		Qty:           st.OrderQty,
		Status:        status,
		TS:            e.clock.Now(),
	}
}

// --- persistence + risk helpers (nil-safe) ---

func (e *MoomooExecutor) persistOrder(ctx context.Context, o domain.Order) {
	if e.cfg.Persist == nil {
		return
	}
	if err := e.cfg.Persist.UpsertOrder(ctx, o); err != nil {
		e.logf("moomoo executor: persist order %s: %v", o.ClientOrderID, err)
	}
}

func (e *MoomooExecutor) persistFill(ctx context.Context, f domain.Fill) {
	if e.cfg.Persist == nil {
		return
	}
	if err := e.cfg.Persist.InsertFill(ctx, f); err != nil {
		e.logf("moomoo executor: persist fill %s: %v", f.TradeID, err)
	}
}

func (e *MoomooExecutor) persistPosition(ctx context.Context, p domain.Position) {
	if e.cfg.Persist == nil {
		return
	}
	if err := e.cfg.Persist.UpsertPosition(ctx, p); err != nil {
		e.logf("moomoo executor: persist position %s/%s: %v", p.StrategyID, p.Symbol, err)
	}
}

func (e *MoomooExecutor) recordRisk(ctx context.Context, strategyID, symbol, rule, detail string) {
	if e.cfg.Risk == nil {
		return
	}
	if err := e.cfg.Risk.RecordRiskEvent(ctx, strategyID, symbol, rule, detail); err != nil {
		e.logf("moomoo executor: record risk event %s: %v", rule, err)
	}
}

// SubmittedCount / RejectedCount / FillsEmitted are telemetry for the cockpit.
func (e *MoomooExecutor) SubmittedCount() int64 { return e.submitted.Load() }
func (e *MoomooExecutor) RejectedCount() int64  { return e.rejected.Load() }
func (e *MoomooExecutor) FillsEmitted() int64   { return e.fillsEmit.Load() }
