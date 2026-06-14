package domain

// order.go defines the execution-side value objects: Order, Fill and
// Position snapshots. These are the Go-native counterparts of the order/fill
// state the Python reference delegates to Nautilus plus the moomoo adapter
// conventions (spec §2.14, §7.4, §7.6).

import (
	"fmt"
	"time"
)

// Order is an immutable snapshot of one order. The reference system submits
// MARKET/GTC orders exclusively with integer share quantities (§7.6); the
// LimitPrice/StopPrice fields exist for forward compatibility and are nil
// for market orders.
//
//   - StrategyID is the ENGINE strategy id (e.g. "SEPARunner-000", §7.7),
//     not the logical intent id ("sepa").
//   - Qty is the absolute order quantity (positive); Side encodes direction.
type Order struct {
	ClientOrderID string      `json:"client_order_id"`
	VenueOrderID  string      `json:"venue_order_id,omitempty"`
	StrategyID    string      `json:"strategy_id"`
	Symbol        string      `json:"symbol"`
	Side          OrderSide   `json:"side"`
	Type          OrderType   `json:"type"`
	TIF           TimeInForce `json:"tif"`
	Qty           Qty         `json:"qty"`
	LimitPrice    *Price      `json:"limit_price"`
	StopPrice     *Price      `json:"stop_price"`
	Status        OrderStatus `json:"status"`
	Reason        string      `json:"reason,omitempty"` // originating signal reason
	TS            time.Time   `json:"ts"`               // submission timestamp (UTC)
}

// NewMarketOrder builds the only order shape the reference system emits:
// MARKET / GTC / integer quantity, status SUBMITTED.
func NewMarketOrder(clientOrderID, strategyID, symbol string, side OrderSide, qty Qty, reason string, ts time.Time) Order {
	return Order{
		ClientOrderID: clientOrderID,
		StrategyID:    strategyID,
		Symbol:        symbol,
		Side:          side,
		Type:          OrderTypeMarket,
		TIF:           TIFGTC,
		Qty:           qty,
		Status:        OrderStatusSubmitted,
		Reason:        reason,
		TS:            ts,
	}
}

// Validate checks the Order invariants.
func (o Order) Validate() error {
	if o.ClientOrderID == "" {
		return fmt.Errorf("%w: order has empty client_order_id", ErrInvalidArgument)
	}
	if o.StrategyID == "" {
		return fmt.Errorf("%w: order %s has empty strategy_id", ErrInvalidArgument, o.ClientOrderID)
	}
	if o.Symbol == "" {
		return fmt.Errorf("%w: order %s has empty symbol", ErrInvalidArgument, o.ClientOrderID)
	}
	if !o.Side.IsValid() {
		return fmt.Errorf("%w: order %s has invalid side %q", ErrInvalidArgument, o.ClientOrderID, string(o.Side))
	}
	if !o.Type.IsValid() {
		return fmt.Errorf("%w: order %s has invalid type %q", ErrInvalidArgument, o.ClientOrderID, string(o.Type))
	}
	if !o.TIF.IsValid() {
		return fmt.Errorf("%w: order %s has invalid tif %q", ErrInvalidArgument, o.ClientOrderID, string(o.TIF))
	}
	if !o.Status.IsValid() {
		return fmt.Errorf("%w: order %s has invalid status %q", ErrInvalidArgument, o.ClientOrderID, string(o.Status))
	}
	if o.Qty <= 0 {
		return fmt.Errorf("%w: order %s has non-positive qty %d", ErrInvalidArgument, o.ClientOrderID, o.Qty)
	}
	switch o.Type {
	case OrderTypeLimit, OrderTypeStopLimit:
		if o.LimitPrice == nil || !o.LimitPrice.IsPositive() {
			return fmt.Errorf("%w: order %s type %s requires a positive limit price",
				ErrInvalidArgument, o.ClientOrderID, o.Type)
		}
	}
	switch o.Type {
	case OrderTypeStopMarket, OrderTypeStopLimit:
		if o.StopPrice == nil || !o.StopPrice.IsPositive() {
			return fmt.Errorf("%w: order %s type %s requires a positive stop price",
				ErrInvalidArgument, o.ClientOrderID, o.Type)
		}
	}
	if o.TS.IsZero() {
		return fmt.Errorf("%w: order %s has zero timestamp", ErrInvalidArgument, o.ClientOrderID)
	}
	return nil
}

// Fill is one execution (delta, not cumulative). The moomoo adapter converts
// the broker's cumulative pushes to deltas before constructing a Fill
// (spec §2.14): delta_qty = cum_qty - prior_qty, last price = delta notional
// / delta qty at 4 decimals — which the 1e-4 Price holds exactly.
//
// TradeID format in the reference: "{venue_order_id}-{ts_ns}". Commission is
// 0 in backtest (zero-fee equity instrument, §7.1).
type Fill struct {
	TradeID       string    `json:"trade_id"`
	ClientOrderID string    `json:"client_order_id"`
	VenueOrderID  string    `json:"venue_order_id,omitempty"`
	StrategyID    string    `json:"strategy_id"`
	Symbol        string    `json:"symbol"`
	Side          OrderSide `json:"side"`
	Qty           Qty       `json:"qty"`   // delta quantity, positive
	Price         Price     `json:"price"` // per-share fill price
	Commission    Money     `json:"commission"`
	TS            time.Time `json:"ts"` // execution timestamp (UTC)
}

// Notional returns Qty × Price as Money (overflow-checked).
func (f Fill) Notional() (Money, error) {
	return f.Price.MulQty(f.Qty)
}

// Validate checks the Fill invariants.
func (f Fill) Validate() error {
	if f.TradeID == "" {
		return fmt.Errorf("%w: fill has empty trade_id", ErrInvalidArgument)
	}
	if f.ClientOrderID == "" {
		return fmt.Errorf("%w: fill %s has empty client_order_id", ErrInvalidArgument, f.TradeID)
	}
	// StrategyID is REQUIRED: a fill with no strategy attribution would settle
	// into accounting under an empty-strategy key, creating an orphan per-strategy
	// position + a spurious reconciliation entry (the post-crash-recovery drift
	// class). Every legitimate fill descends from an Order (whose Validate already
	// requires a strategy id) or a recovery seed (which carries a recovery
	// pseudo-strategy), so this never rejects a correctly-attributed fill — it
	// fails loud on the mis-attributed one instead of drifting silently.
	if f.StrategyID == "" {
		return fmt.Errorf("%w: fill %s has empty strategy_id", ErrInvalidArgument, f.TradeID)
	}
	if f.Symbol == "" {
		return fmt.Errorf("%w: fill %s has empty symbol", ErrInvalidArgument, f.TradeID)
	}
	if !f.Side.IsValid() {
		return fmt.Errorf("%w: fill %s has invalid side %q", ErrInvalidArgument, f.TradeID, string(f.Side))
	}
	if f.Qty <= 0 {
		return fmt.Errorf("%w: fill %s has non-positive qty %d (deltas only, duplicates dropped)",
			ErrInvalidArgument, f.TradeID, f.Qty)
	}
	if !f.Price.IsPositive() {
		return fmt.Errorf("%w: fill %s has non-positive price %s", ErrInvalidArgument, f.TradeID, f.Price)
	}
	if f.Commission.IsNegative() {
		return fmt.Errorf("%w: fill %s has negative commission %s", ErrInvalidArgument, f.TradeID, f.Commission)
	}
	if f.TS.IsZero() {
		return fmt.Errorf("%w: fill %s has zero timestamp", ErrInvalidArgument, f.TradeID)
	}
	return nil
}

// Position is an immutable snapshot of one netted position. Per the NETTING
// OMS semantics (§7.4 [MUST-MATCH]) there is exactly one position per
// (strategy, instrument); two strategies trading the same symbol hold two
// separate Position values, and cross-strategy netting is computed over them
// (AccountSnapshot.NetPositionAcrossStrategies).
//
// SignedQty: positive = long, negative = short, 0 = flat. The Python
// platform truncates Nautilus's float signed_qty toward zero; Go positions
// are integral from the start (QtyFromFloat64Trunc exists for any boundary
// that still receives floats).
type Position struct {
	StrategyID  string    `json:"strategy_id"` // engine strategy id (§7.7)
	Symbol      string    `json:"symbol"`
	SignedQty   Qty       `json:"signed_qty"`
	AvgPx       Price     `json:"avg_px"` // average entry price; 0 when flat
	RealizedPnL Money     `json:"realized_pnl"`
	UpdatedAt   time.Time `json:"updated_at"` // ts of the last applied fill (UTC)
}

// IsFlat reports SignedQty == 0.
func (p Position) IsFlat() bool { return p.SignedQty == 0 }

// IsLong reports SignedQty > 0.
func (p Position) IsLong() bool { return p.SignedQty > 0 }

// IsShort reports SignedQty < 0.
func (p Position) IsShort() bool { return p.SignedQty < 0 }

// MarketValue returns SignedQty × last as signed Money (overflow-checked).
func (p Position) MarketValue(last Price) (Money, error) {
	return last.MulQty(p.SignedQty)
}

// Validate checks the Position invariants.
func (p Position) Validate() error {
	if p.StrategyID == "" {
		return fmt.Errorf("%w: position has empty strategy_id", ErrInvalidArgument)
	}
	if p.Symbol == "" {
		return fmt.Errorf("%w: position %s has empty symbol", ErrInvalidArgument, p.StrategyID)
	}
	if p.AvgPx.IsNegative() {
		return fmt.Errorf("%w: position %s/%s has negative avg price %s",
			ErrInvalidArgument, p.StrategyID, p.Symbol, p.AvgPx)
	}
	return nil
}
