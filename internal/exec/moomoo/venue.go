package moomoo

// venue.go is an IN-MEMORY mock trading venue implementing mo.TradeClient — the
// deterministic gate driver for the MoomooExecutor (locked decision 9). It is
// PERMANENT, reusable test/sim infrastructure (not _test.go) so the executor's
// behaviour can be proven without an OpenD connection, with full control over
// accept/fill/reject/partial/cancel sequencing.
//
// FAITHFULNESS: it speaks the SAME normalised TradeClient surface the wire
// client implements (PlaceOrder + Trd_UpdateOrder/UpdateOrderFill pushes via the
// registered handlers, GetPositionList/GetFunds/GetAccList/GetOrderList), with
// the SAME cumulative-fill semantics (DealtQty/DealtAvgPrice are cumulative) and
// the SAME status set. A behaviour that is green here is built to be green
// against the real wire client + real OpenD, so this is the deterministic gate.
//
// Determinism: every push is delivered synchronously on the calling goroutine
// (a test drives accept/fill explicitly via Accept/Fill/Reject/Cancel), so there
// is no clock or scheduler nondeterminism. A test asserts exact ordering.

import (
	"context"
	"fmt"
	"sort"
	"sync"

	mo "github.com/byjackchen/trade-tms-go/internal/adapters/moomoo"
	"github.com/byjackchen/trade-tms-go/internal/adapters/moomoo/pb/trdcommon"
	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// MockVenue is a controllable in-memory TradeClient. Build with NewMockVenue,
// register it as the executor's Client, then drive order lifecycle transitions
// with Accept / Fill / PartialFill / Reject / Cancel.
type MockVenue struct {
	mu sync.Mutex

	// accounts the venue exposes per env (GetAccList).
	accounts map[mo.TrdEnv][]uint64
	// unlocked tracks whether UnlockTrade(REAL) has been called (live gate).
	unlocked bool
	// unlockPassword, if non-empty, is required by UnlockTrade(REAL).
	unlockPassword string
	// failPlace, when set, makes the next PlaceOrder return this error (reject at
	// submit time, e.g. insufficient buying power / bad symbol).
	failPlace error

	orderH mo.TrdOrderHandler
	fillH  mo.TrdOrderFillHandler

	seq    int
	orders map[string]*mockOrder // venueOrderID -> order
	byCOID map[string]string     // clientOrderID -> venueOrderID
	posns  map[string]*mockPos   // symbol -> netted position
	funds  mo.Funds
}

type mockOrder struct {
	venueID string
	coid    string
	symbol  string
	side    domain.OrderSide
	qty     domain.Qty
	cumQty  domain.Qty
	cumNotn int64 // 1e-4 money units
	status  trdcommon.OrderStatus
	lastErr string
}

type mockPos struct {
	qty domain.Qty // signed
	// cost basis tracking for avg price (1e-4 notional of the open side).
	costNotnAbs int64
}

// NewMockVenue builds a venue with a paper (SIMULATE) account id pre-registered.
func NewMockVenue(paperAccID uint64) *MockVenue {
	v := &MockVenue{
		accounts: map[mo.TrdEnv][]uint64{mo.TrdEnvSimulate: {paperAccID}},
		orders:   make(map[string]*mockOrder),
		byCOID:   make(map[string]string),
		posns:    make(map[string]*mockPos),
		funds: mo.Funds{
			TotalAssets:    domain.MustMoney("100000.00"),
			Cash:           domain.MustMoney("100000.00"),
			AvailableFunds: domain.MustMoney("100000.00"),
		},
	}
	return v
}

// RegisterRealAccount adds a REAL account id (for live-path tests). The unlock
// password, if non-empty, is then required by UnlockTrade.
func (v *MockVenue) RegisterRealAccount(realAccID uint64, unlockPassword string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.accounts[mo.TrdEnvReal] = append(v.accounts[mo.TrdEnvReal], realAccID)
	v.unlockPassword = unlockPassword
}

// FailNextPlace makes the next PlaceOrder fail with err (a submit-time reject).
func (v *MockVenue) FailNextPlace(err error) {
	v.mu.Lock()
	v.failPlace = err
	v.mu.Unlock()
}

// SetPosition forces the venue's net position in symbol to signedQty at avgPrice
// (positive long, negative short, 0 flat). It is the gate driver for
// reconciliation-mismatch + crash-recovery tests: it simulates a broker position
// that diverges from the strategy book (a missed fill) or pre-exists a fresh
// session (a prior run's open). It does NOT push an order update — it only
// changes the GetPositionList truth, exactly as a broker-side change would.
func (v *MockVenue) SetPosition(symbol string, signedQty domain.Qty, avgPrice domain.Price) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if signedQty == 0 {
		delete(v.posns, symbol)
		return
	}
	abs := signedQty
	if abs < 0 {
		abs = -abs
	}
	notn, _ := avgPrice.MulQty(abs)
	v.posns[symbol] = &mockPos{qty: signedQty, costNotnAbs: abs64(notn.Raw())}
}

// Unlocked reports whether UnlockTrade(REAL) has been called successfully.
func (v *MockVenue) Unlocked() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.unlocked
}

// --- mo.TradeClient implementation ---

func (v *MockVenue) GetAccList(_ context.Context, env mo.TrdEnv) ([]mo.TradeAccount, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	var out []mo.TradeAccount
	for _, id := range v.accounts[env] {
		out = append(out, mo.TradeAccount{AccID: id, TrdEnv: env})
	}
	return out, nil
}

func (v *MockVenue) UnlockTrade(_ context.Context, env mo.TrdEnv, password string, _ int32) error {
	if env != mo.TrdEnvReal {
		return nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.unlockPassword != "" && password != v.unlockPassword {
		return fmt.Errorf("mock venue: unlock password mismatch")
	}
	v.unlocked = true
	return nil
}

func (v *MockVenue) SubscribeOrderUpdates(h mo.TrdOrderHandler) error {
	v.mu.Lock()
	v.orderH = h
	v.mu.Unlock()
	return nil
}

func (v *MockVenue) SubscribeFillUpdates(h mo.TrdOrderFillHandler) error {
	v.mu.Lock()
	v.fillH = h
	v.mu.Unlock()
	return nil
}

func (v *MockVenue) PlaceOrder(_ context.Context, req mo.PlaceOrderRequest) (mo.PlaceOrderResult, error) {
	if err := req.Validate(); err != nil {
		return mo.PlaceOrderResult{}, err
	}
	v.mu.Lock()
	defer v.mu.Unlock()

	// SAFETY check at the venue too: a REAL order requires a prior unlock.
	if req.TrdEnv == mo.TrdEnvReal && !v.unlocked {
		return mo.PlaceOrderResult{}, fmt.Errorf("mock venue: REAL order before UnlockTrade")
	}
	if v.failPlace != nil {
		err := v.failPlace
		v.failPlace = nil
		return mo.PlaceOrderResult{}, err
	}
	// Idempotency: a repeat of a known client-order-id returns the existing venue
	// id WITHOUT creating a second order.
	if vid, ok := v.byCOID[req.ClientOrderID]; ok {
		return mo.PlaceOrderResult{VenueOrderID: vid}, nil
	}
	v.seq++
	vid := fmt.Sprintf("V%d", v.seq)
	v.orders[vid] = &mockOrder{
		venueID: vid,
		coid:    req.ClientOrderID,
		symbol:  req.Symbol,
		side:    req.Side,
		qty:     req.Qty,
		status:  trdcommon.OrderStatus_OrderStatus_WaitingSubmit,
	}
	v.byCOID[req.ClientOrderID] = vid
	return mo.PlaceOrderResult{VenueOrderID: vid}, nil
}

// CancelOrder cancels a working order by client-order-id (mo.TradeClient). It
// pushes a terminal CANCELLED_ALL via the order handler (the same path the real
// venue uses to confirm a cancel), so the executor's state machine marks the
// order CANCELED. Idempotent + a no-op for an unknown or already-terminal order:
// a double-cancel never errors, matching the manual desk's idempotency contract.
func (v *MockVenue) CancelOrder(_ context.Context, _ uint64, _ mo.TrdEnv, clientOrderID string) error {
	v.mu.Lock()
	o, ok := v.orderByCOID(clientOrderID)
	if !ok {
		v.mu.Unlock()
		return nil // unknown order: no-op (idempotent)
	}
	switch o.status {
	case trdcommon.OrderStatus_OrderStatus_Filled_All,
		trdcommon.OrderStatus_OrderStatus_Cancelled_All,
		trdcommon.OrderStatus_OrderStatus_Failed:
		v.mu.Unlock()
		return nil // already terminal: no-op (idempotent)
	}
	o.status = trdcommon.OrderStatus_OrderStatus_Cancelled_All
	upd := v.updateOf(o)
	oh := v.orderH
	v.mu.Unlock()
	if oh != nil {
		oh(upd)
	}
	return nil
}

func (v *MockVenue) GetOrderList(_ context.Context, _ uint64, _ mo.TrdEnv) ([]mo.OrderUpdate, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]mo.OrderUpdate, 0, len(v.orders))
	for _, o := range v.orders {
		out = append(out, v.updateOf(o))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].VenueOrderID < out[j].VenueOrderID })
	return out, nil
}

func (v *MockVenue) GetPositionList(_ context.Context, _ uint64, _ mo.TrdEnv) ([]mo.BrokerPosition, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]mo.BrokerPosition, 0, len(v.posns))
	for sym, p := range v.posns {
		if p.qty == 0 {
			continue
		}
		var avg domain.Price
		if p.qty != 0 {
			abs := p.qty
			if abs < 0 {
				abs = -abs
			}
			avg = domain.Price(p.costNotnAbs / int64(abs))
		}
		out = append(out, mo.BrokerPosition{Symbol: sym, Qty: p.qty, AvgPrice: avg})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Symbol < out[j].Symbol })
	return out, nil
}

func (v *MockVenue) GetFunds(_ context.Context, _ uint64, _ mo.TrdEnv) (mo.Funds, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.funds, nil
}

func (v *MockVenue) GetOrderFillList(_ context.Context, _ uint64, _ mo.TrdEnv) ([]mo.FillUpdate, error) {
	return nil, nil
}

// --- controllable lifecycle drivers (test-facing) ---

// Accept pushes a SUBMITTED (accepted) update for the order with clientOrderID.
func (v *MockVenue) Accept(clientOrderID string) error {
	return v.transition(clientOrderID, trdcommon.OrderStatus_OrderStatus_Submitted, 0, 0)
}

// Fill pushes a full fill at price: cumulative qty = order qty, status
// FILLED_ALL. It also updates the venue's net position + a fill push.
func (v *MockVenue) Fill(clientOrderID string, price domain.Price) error {
	v.mu.Lock()
	o, ok := v.orderByCOID(clientOrderID)
	if !ok {
		v.mu.Unlock()
		return fmt.Errorf("mock venue: unknown order %s", clientOrderID)
	}
	full := o.qty
	v.mu.Unlock()
	return v.PartialFill(clientOrderID, full, price)
}

// PartialFill pushes a fill bringing the CUMULATIVE filled qty up by addQty at
// price. When cumulative reaches the order qty, the status is FILLED_ALL,
// otherwise FILLED_PART. DealtAvgPrice is the cumulative average. It mirrors the
// real venue's cumulative-fill semantics.
func (v *MockVenue) PartialFill(clientOrderID string, addQty domain.Qty, price domain.Price) error {
	v.mu.Lock()
	o, ok := v.orderByCOID(clientOrderID)
	if !ok {
		v.mu.Unlock()
		return fmt.Errorf("mock venue: unknown order %s", clientOrderID)
	}
	addNotn, err := price.MulQty(addQty)
	if err != nil {
		v.mu.Unlock()
		return err
	}
	o.cumQty += addQty
	o.cumNotn += addNotn.Raw()
	if o.cumQty >= o.qty {
		o.cumQty = o.qty
		o.status = trdcommon.OrderStatus_OrderStatus_Filled_All
	} else {
		o.status = trdcommon.OrderStatus_OrderStatus_Filled_Part
	}
	// Update venue net position (signed by side).
	v.applyVenueFill(o.symbol, o.side, addQty, price)
	upd := v.updateOf(o)
	fill := mo.FillUpdate{
		FillID:        fmt.Sprintf("%s-F%d", o.venueID, o.cumQty),
		ClientOrderID: o.coid,
		VenueOrderID:  o.venueID,
		Symbol:        o.symbol,
		Side:          o.side,
		Qty:           addQty,
		Price:         price,
	}
	oh, fh := v.orderH, v.fillH
	v.mu.Unlock()

	if fh != nil {
		fh(fill)
	}
	if oh != nil {
		oh(upd)
	}
	return nil
}

// Reject pushes a terminal reject (FAILED) with reason.
func (v *MockVenue) Reject(clientOrderID, reason string) error {
	v.mu.Lock()
	o, ok := v.orderByCOID(clientOrderID)
	if !ok {
		v.mu.Unlock()
		return fmt.Errorf("mock venue: unknown order %s", clientOrderID)
	}
	o.status = trdcommon.OrderStatus_OrderStatus_Failed
	o.lastErr = reason
	upd := v.updateOf(o)
	oh := v.orderH
	v.mu.Unlock()
	if oh != nil {
		oh(upd)
	}
	return nil
}

// Cancel pushes a terminal cancel (CANCELLED_ALL).
func (v *MockVenue) Cancel(clientOrderID string) error {
	return v.transition(clientOrderID, trdcommon.OrderStatus_OrderStatus_Cancelled_All, 0, 0)
}

// PushRaw re-pushes the order's current state (for idempotency / duplicate-push
// tests) without changing it.
func (v *MockVenue) PushRaw(clientOrderID string) error {
	v.mu.Lock()
	o, ok := v.orderByCOID(clientOrderID)
	if !ok {
		v.mu.Unlock()
		return fmt.Errorf("mock venue: unknown order %s", clientOrderID)
	}
	upd := v.updateOf(o)
	oh := v.orderH
	v.mu.Unlock()
	if oh != nil {
		oh(upd)
	}
	return nil
}

// transition sets a non-fill status and pushes it. addQty/price unused for
// non-fill states.
func (v *MockVenue) transition(clientOrderID string, status trdcommon.OrderStatus, _ domain.Qty, _ domain.Price) error {
	v.mu.Lock()
	o, ok := v.orderByCOID(clientOrderID)
	if !ok {
		v.mu.Unlock()
		return fmt.Errorf("mock venue: unknown order %s", clientOrderID)
	}
	o.status = status
	upd := v.updateOf(o)
	oh := v.orderH
	v.mu.Unlock()
	if oh != nil {
		oh(upd)
	}
	return nil
}

// orderByCOID resolves a tracked order by client-order-id (caller holds mu).
func (v *MockVenue) orderByCOID(coid string) (*mockOrder, bool) {
	vid, ok := v.byCOID[coid]
	if !ok {
		return nil, false
	}
	o, ok := v.orders[vid]
	return o, ok
}

// updateOf builds a normalised OrderUpdate from a mock order (caller holds mu).
func (v *MockVenue) updateOf(o *mockOrder) mo.OrderUpdate {
	var avg domain.Price
	if o.cumQty > 0 {
		avg = domain.Price(o.cumNotn / int64(o.cumQty))
	}
	return mo.OrderUpdate{
		ClientOrderID: o.coid,
		VenueOrderID:  o.venueID,
		Symbol:        o.symbol,
		Side:          o.side,
		RawStatus:     int32(o.status),
		OrderQty:      o.qty,
		DealtQty:      o.cumQty,
		DealtAvgPrice: avg,
		LastErrMsg:    o.lastErr,
	}
}

// applyVenueFill nets a fill into the venue position book (caller holds mu).
func (v *MockVenue) applyVenueFill(symbol string, side domain.OrderSide, qty domain.Qty, price domain.Price) {
	p := v.posns[symbol]
	if p == nil {
		p = &mockPos{}
		v.posns[symbol] = p
	}
	signed := qty
	if side == domain.OrderSideSell {
		signed = -qty
	}
	notn, _ := price.MulQty(qty)
	// Reduce or grow the position; track cost notional on the OPEN side only.
	if (p.qty >= 0 && signed > 0) || (p.qty <= 0 && signed < 0) {
		// growing
		p.costNotnAbs += abs64(notn.Raw())
	} else {
		// reducing: shrink cost basis proportionally
		absPrev := p.qty
		if absPrev < 0 {
			absPrev = -absPrev
		}
		if absPrev > 0 {
			reduce := qty
			if reduce > absPrev {
				reduce = absPrev
			}
			p.costNotnAbs -= p.costNotnAbs * int64(reduce) / int64(absPrev)
		}
	}
	p.qty += signed
	if p.qty == 0 {
		p.costNotnAbs = 0
	}
}

func abs64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

// compile-time assertion: *MockVenue implements mo.TradeClient.
var _ mo.TradeClient = (*MockVenue)(nil)
