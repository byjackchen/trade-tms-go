package mock

// venue.go extends the protocol-faithful mock OpenD with a MOCK TRADING VENUE
// (P6 locked decision 9): it accepts Trd_PlaceOrder, simulates accept->fill or
// reject, pushes Trd_UpdateOrder + Trd_UpdateOrderFill, and serves mock
// positions/funds via Trd_GetPositionList / Trd_GetFunds — for BOTH a paper
// (TrdEnv_Simulate) and a 'live' (TrdEnv_Real, but still fake) account.
//
// It speaks the IDENTICAL Trd_* wire messages as a real FutuOpenD trading
// server, so the native trading client (parent package) cannot tell it apart at
// the wire level: green-on-mock predicts green-on-real.
//
// FILL MODEL (documented, deterministic): a market order is ACCEPTED
// synchronously (Trd_PlaceOrder reply carries the venue orderID; an immediate
// Trd_UpdateOrder push reports status Submitted), then FILLED at the NEXT pushed
// bar for its symbol — the close of the next Qot_UpdateKL bar is the fill price.
// This is the SAME "fill at the next observed bar" model the engine's realistic
// SimExecutor uses, so a strategy's mock-traded fills line up with its backtest
// fills. Fills push Trd_UpdateOrderFill (per-execution) followed by
// Trd_UpdateOrder (cumulative, status Filled_All). All ids
// (orderID/fillID/positionID) come from a single monotonic counter seeded by the
// server, so a replayed session reproduces identical ids — deterministic under
// the controllable clock (Options.Now + PushKLine drive everything).
//
// REJECTS are deterministic too: unknown symbol (not in the BarSource),
// insufficient buying power (notional > funds.power), or market-closed (a flag a
// test can set) -> Trd_PlaceOrder retType!=0 AND a terminal Trd_UpdateOrder push
// with status SubmitFailed and lastErrMsg. No fill, no position change.

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	mo "github.com/byjackchen/trade-tms-go/internal/broker/moomoo"
	"github.com/byjackchen/trade-tms-go/internal/broker/moomoo/pb/trdcommon"
	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// VenueRejectReason classifies a deterministic order rejection.
type VenueRejectReason string

const (
	RejectUnknownSymbol  VenueRejectReason = "unknown symbol"
	RejectInsufficientBP VenueRejectReason = "insufficient buying power"
	RejectMarketClosed   VenueRejectReason = "market closed"
)

// VenueAccount is one mock trading account (paper or fake-live). Its positions
// and funds are maintained by the venue as orders fill.
type venueAccount struct {
	env     mo.TrdEnv
	accID   uint64
	accType trdcommon.TrdAccType

	// power is the long buying power (cash available to open new longs). It is
	// debited on a buy fill and credited on a sell fill (mark-to-fill).
	power float64
	cash  float64

	// positions keyed by symbol; qty signed (long>0, short<0).
	positions map[string]*venuePosition
}

type venuePosition struct {
	positionID uint64
	symbol     string
	qty        float64 // signed
	costPrice  float64 // average cost of the open position
}

// venueOrder is a working order awaiting its next-bar fill.
type venueOrder struct {
	orderID       uint64
	clientOrderID string // from the PlaceOrder remark
	env           mo.TrdEnv
	accID         uint64
	symbol        string
	side          trdcommon.TrdSide
	qty           float64
	createTS      time.Time
}

// tradeVenue is the mock venue state attached to a Server.
type tradeVenue struct {
	mu sync.Mutex

	accounts map[uint64]*venueAccount // by accID

	// idSeq is the single deterministic id source (orderID/fillID/positionID).
	idSeq uint64

	// working orders awaiting a next-bar fill, in submission order, keyed by
	// symbol for the push-driven fill.
	workingBySymbol map[string][]*venueOrder

	// filledOrders is the history of fully-filled orders (so Trd_GetOrderList
	// reports them as Filled_All after the fill, like a real venue) + filledFills
	// is the per-execution fill history (so Trd_GetOrderFillList — the DIRECTION-2
	// sync read — returns them). Both are append-only and keyed by accID at query.
	filledOrders []*filledOrder
	filledFills  []*filledFill

	// marketClosed, when true, rejects new orders with RejectMarketClosed.
	marketClosed bool
}

// filledOrder is a fully-filled order retained for Trd_GetOrderList history.
type filledOrder struct {
	o       *venueOrder
	fillTS  time.Time
	fillAvg float64
	fillQty float64
}

// filledFill is one execution retained for Trd_GetOrderFillList history.
type filledFill struct {
	o      *venueOrder
	fillID uint64
	price  float64
	ts     time.Time
}

// VenueConfig configures the mock trading venue's accounts and starting funds.
type VenueConfig struct {
	// PaperAccID is the simulate (paper) account id; required to enable paper.
	PaperAccID uint64
	// LiveAccID is the real (still fake) account id; required to enable 'live'.
	LiveAccID uint64
	// StartingPower is each account's initial long buying power (and cash).
	StartingPower float64
}

// EnableTrading attaches a mock trading venue to the server with the given
// accounts and starting funds. Call before Serve. Idempotent re-config replaces
// the venue.
func (s *Server) EnableTrading(cfg VenueConfig) {
	v := &tradeVenue{
		accounts:        make(map[uint64]*venueAccount),
		workingBySymbol: make(map[string][]*venueOrder),
	}
	power := cfg.StartingPower
	if power <= 0 {
		power = 1_000_000 // a generous default so most test orders accept
	}
	if cfg.PaperAccID != 0 {
		v.accounts[cfg.PaperAccID] = newVenueAccount(mo.TrdEnvSimulate, cfg.PaperAccID, power)
	}
	if cfg.LiveAccID != 0 {
		v.accounts[cfg.LiveAccID] = newVenueAccount(mo.TrdEnvReal, cfg.LiveAccID, power)
	}
	s.mu.Lock()
	s.venue = v
	s.mu.Unlock()
}

func newVenueAccount(env mo.TrdEnv, accID uint64, power float64) *venueAccount {
	return &venueAccount{
		env:       env,
		accID:     accID,
		accType:   trdcommon.TrdAccType_TrdAccType_Margin,
		power:     power,
		cash:      power,
		positions: make(map[string]*venuePosition),
	}
}

// SetMarketClosed toggles the market-closed reject path (a test calls it to
// drive the market-closed rejection deterministically).
func (s *Server) SetMarketClosed(closed bool) {
	s.mu.Lock()
	v := s.venue
	s.mu.Unlock()
	if v == nil {
		return
	}
	v.mu.Lock()
	v.marketClosed = closed
	v.mu.Unlock()
}

// nextID returns a fresh deterministic id under the venue lock (caller holds it).
func (v *tradeVenue) nextIDLocked() uint64 {
	v.idSeq++
	return v.idSeq
}

// VenuePosition is a read-only snapshot of one mock position (for test asserts).
type VenuePosition struct {
	Symbol    string
	Qty       domain.Qty
	CostPrice domain.Price
}

// VenuePositions returns the mock venue's positions for accID (test inspection).
func (s *Server) VenuePositions(accID uint64) []VenuePosition {
	s.mu.Lock()
	v := s.venue
	s.mu.Unlock()
	if v == nil {
		return nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	acc := v.accounts[accID]
	if acc == nil {
		return nil
	}
	out := make([]VenuePosition, 0, len(acc.positions))
	for _, p := range acc.positions {
		if p.qty == 0 {
			continue
		}
		q, _ := domain.QtyFromFloat64Trunc(p.qty)
		cp, _ := domain.PriceFromFloat64(p.costPrice)
		out = append(out, VenuePosition{Symbol: p.symbol, Qty: q, CostPrice: cp})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Symbol < out[j].Symbol })
	return out
}

// pendingSymbols returns the set of symbols that currently have at least one
// working (un-filled) order, so the autonomous fill driver (the standalone
// mock-opend) knows which symbols to price + fill without a PushKLine tick. The
// returned slice is a snapshot under the venue lock.
func (v *tradeVenue) pendingSymbols() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	if len(v.workingBySymbol) == 0 {
		return nil
	}
	out := make([]string, 0, len(v.workingBySymbol))
	for sym, orders := range v.workingBySymbol {
		if len(orders) > 0 {
			out = append(out, sym)
		}
	}
	return out
}

// fillWorkingOrders fills every working order for symbol against fillPrice (the
// close of the just-pushed bar), updating positions/funds and returning the
// (UpdateOrderFill, UpdateOrder) push payloads to deliver. Called from the
// server's PushKLine on the controllable clock. ts is the bar's timestamp.
func (v *tradeVenue) fillWorkingOrders(symbol string, fillPrice float64, ts time.Time) []func(*conn) error {
	v.mu.Lock()
	orders := v.workingBySymbol[symbol]
	if len(orders) == 0 {
		v.mu.Unlock()
		return nil
	}
	delete(v.workingBySymbol, symbol)

	var pushes []func(*conn) error
	for _, o := range orders {
		acc := v.accounts[o.accID]
		if acc == nil {
			continue
		}
		fillID := v.nextIDLocked()
		signed := o.qty
		if o.side == trdcommon.TrdSide_TrdSide_Sell {
			signed = -o.qty
		}
		applyFillToAccount(acc, v, o.symbol, signed, fillPrice)

		// Retain the fill + the now-Filled order in history so the broker reads
		// (Trd_GetOrderList / Trd_GetOrderFillList — the DIRECTION-2 sync) report
		// them after the order leaves workingBySymbol, exactly as a real venue does.
		v.filledFills = append(v.filledFills, &filledFill{o: o, fillID: fillID, price: fillPrice, ts: ts})
		v.filledOrders = append(v.filledOrders, &filledOrder{o: o, fillTS: ts, fillAvg: fillPrice, fillQty: o.qty})

		// Build the two pushes for this fill: per-execution OrderFill, then the
		// cumulative Order at Filled_All.
		fillPush := buildFillPush(o, fillID, fillPrice, ts)
		orderPush := buildOrderPush(o, fillPrice, ts, trdcommon.OrderStatus_OrderStatus_Filled_All, o.qty, fillPrice, "")
		pushes = append(pushes, fillPush, orderPush)
	}
	v.mu.Unlock()
	return pushes
}

// applyFillToAccount mutates the account's position + funds for a signed fill at
// price. Buying power: debit notional on a net buy, credit on a net sell
// (simple cash model — sufficient for the gate; a real margin model is out of
// scope for the mock).
func applyFillToAccount(acc *venueAccount, v *tradeVenue, symbol string, signedQty, price float64) {
	pos := acc.positions[symbol]
	if pos == nil {
		pos = &venuePosition{positionID: v.nextIDLocked(), symbol: symbol}
		acc.positions[symbol] = pos
	}
	prevQty := pos.qty
	newQty := prevQty + signedQty
	notional := signedQty * price
	acc.power -= notional
	acc.cash -= notional

	switch {
	case prevQty == 0 || (prevQty > 0) == (signedQty > 0):
		// Opening or increasing in the same direction: weighted-average cost.
		if newQty != 0 {
			pos.costPrice = (prevQty*pos.costPrice + signedQty*price) / newQty
		}
	case (prevQty > 0) != (newQty > 0) && newQty != 0:
		// Crossed through zero (reverse): the remainder opens at the fill price.
		pos.costPrice = price
	}
	pos.qty = newQty
	if pos.qty == 0 {
		pos.costPrice = 0
	}
}

// buildFillPush returns a closure that pushes a Trd_UpdateOrderFill for one fill.
func buildFillPush(o *venueOrder, fillID uint64, price float64, ts time.Time) func(*conn) error {
	return func(c *conn) error {
		return c.pushTrdFill(o, fillID, price, ts)
	}
}

// buildOrderPush returns a closure that pushes a Trd_UpdateOrder at the given
// cumulative status/fill.
func buildOrderPush(o *venueOrder, price float64, ts time.Time, status trdcommon.OrderStatus, fillQty, fillAvg float64, errMsg string) func(*conn) error {
	return func(c *conn) error {
		return c.pushTrdOrder(o, ts, status, fillQty, fillAvg, errMsg)
	}
}

// orderToTrd builds a Trd_Common.Order for a venue order at a given status.
func orderToTrd(o *venueOrder, ts time.Time, status trdcommon.OrderStatus, fillQty, fillAvg float64, errMsg string) *trdcommon.Order {
	return &trdcommon.Order{
		TrdSide:         proto.Int32(int32(o.side)),
		OrderType:       proto.Int32(int32(trdcommon.OrderType_OrderType_Market)),
		OrderStatus:     proto.Int32(int32(status)),
		OrderID:         proto.Uint64(o.orderID),
		OrderIDEx:       proto.String(fmt.Sprintf("%d", o.orderID)),
		Code:            proto.String(o.symbol),
		Name:            proto.String(o.symbol),
		Qty:             proto.Float64(o.qty),
		Price:           proto.Float64(0), // market order: no limit price
		CreateTime:      proto.String(mo.FormatTrdTime(o.createTS)),
		UpdateTime:      proto.String(mo.FormatTrdTime(ts)),
		FillQty:         proto.Float64(fillQty),
		FillAvgPrice:    proto.Float64(fillAvg),
		LastErrMsg:      proto.String(errMsg),
		SecMarket:       proto.Int32(mo.TrdSecMarketUS),
		CreateTimestamp: proto.Float64(float64(o.createTS.Unix())),
		UpdateTimestamp: proto.Float64(float64(ts.Unix())),
		Remark:          proto.String(o.clientOrderID),
		TrdMarket:       proto.Int32(mo.TrdMarketUS),
	}
}

// fillToTrd builds a Trd_Common.OrderFill for one execution.
func fillToTrd(o *venueOrder, fillID uint64, price float64, ts time.Time) *trdcommon.OrderFill {
	return &trdcommon.OrderFill{
		TrdSide:         proto.Int32(int32(o.side)),
		FillID:          proto.Uint64(fillID),
		FillIDEx:        proto.String(fmt.Sprintf("%d", fillID)),
		OrderID:         proto.Uint64(o.orderID),
		OrderIDEx:       proto.String(fmt.Sprintf("%d", o.orderID)),
		Code:            proto.String(o.symbol),
		Name:            proto.String(o.symbol),
		Qty:             proto.Float64(o.qty),
		Price:           proto.Float64(price),
		CreateTime:      proto.String(mo.FormatTrdTime(ts)),
		SecMarket:       proto.Int32(mo.TrdSecMarketUS),
		CreateTimestamp: proto.Float64(float64(ts.Unix())),
		UpdateTimestamp: proto.Float64(float64(ts.Unix())),
		Status:          proto.Int32(int32(trdcommon.OrderFillStatus_OrderFillStatus_OK)),
		TrdMarket:       proto.Int32(mo.TrdMarketUS),
	}
}
