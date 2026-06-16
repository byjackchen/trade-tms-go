package moomoo

// trade.go is the COORDINATION CONTRACT between the native Trd_* wire client
// (trd_client.go, which speaks Trd_GetAccList / Trd_UnlockTrade /
// Trd_PlaceOrder / Trd_GetOrderList + Trd_UpdateOrder push /
// Trd_GetPositionList / Trd_GetFunds / Trd_GetOrderFillList +
// Trd_UpdateOrderFill push) and the execution layer (internal/exec/moomoo).
//
// It defines: (1) the executor-facing PlaceOrderRequest / PlaceOrderResult,
// (2) the normalised push value types OrderUpdate / FillUpdate (decoded from the
// protobuf in trd_client.go so the executor never sees a pb type), (3) the push
// handler signatures, and (4) the TradeClient interface the executor depends on
// — so the wire client and the in-memory mock venue are interchangeable at this
// seam.
//
// SAFETY (locked decision 8): every money-moving method takes an explicit
// AccID + TrdEnv. There is no ambient/default account. PlaceOrder on TrdEnvReal
// is the ONLY real-money path; the executor refuses to construct it without the
// full live-activation gate.
//
// TrdEnv, TrdEnvSimulate/Real, BrokerPosition, OrderStatusClass, the
// classifyTrdStatus / DomainOrderStatus helpers and the side mappings live in
// trd_convert.go; this file reuses them.

import (
	"context"
	"errors"
	"fmt"

	"github.com/byjackchen/trade-tms-go/internal/adapters/moomoo/pb/trdcommon"
	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// ErrUnsupported is returned by a TradeClient method a particular client does
// not implement (e.g. the market-data-only P5 surface, when trading is off).
var ErrUnsupported = errors.New("moomoo: trade operation not supported by this client")

// ErrOrderRejected is returned by PlaceOrder when the BROKER/VENUE rejects the
// order for a business reason (insufficient buying power, market closed, unknown
// symbol, bad lot size, ...): the venue replies retType!=0 with a reason message
// and NO order is placed. This is an EXPECTED, operator-facing outcome — NOT an
// internal/transport error — so the manual-desk API maps it to a clean 4xx (422)
// rather than a 500 with a leaked protocol string (finding 4). It is distinct from
// a transport/protocol failure (a dropped connection, a malformed frame), which
// stays an internal error. The wrapped error carries the venue's reason text.
var ErrOrderRejected = errors.New("moomoo: order rejected by the broker")

// PlaceOrderRequest is the executor-facing order submission. trd_client.go
// translates it to a Trd_PlaceOrder C2S (header acc/env, security, qty, price,
// side, market order type, GTC). ClientOrderID is carried through (as the
// order remark) so push correlation + idempotency work before the venue order
// id is known.
//
// IDEMPOTENCY: ClientOrderID is the dedupe key. trd_client.go stamps the
// Trd_PlaceOrder remark from it and maintains a client-order-id -> venue-order
// map, so a reconnect/retry of the SAME ClientOrderID never double-submits at
// the broker. The executor guarantees exactly one PlaceOrder call per
// ClientOrderID; the client guarantees one venue order per ClientOrderID even
// across transport retries.
type PlaceOrderRequest struct {
	AccID         uint64
	TrdEnv        TrdEnv
	ClientOrderID string
	Symbol        string
	Side          domain.OrderSide
	Type          domain.OrderType
	TIF           domain.TimeInForce
	Qty           domain.Qty
	// Price is the limit price for non-market orders; ignored (0) for MARKET.
	Price domain.Price
}

// Validate checks the request invariants before it reaches the wire.
func (r PlaceOrderRequest) Validate() error {
	if r.ClientOrderID == "" {
		return fmt.Errorf("%w: place order has empty client_order_id", domain.ErrInvalidArgument)
	}
	if r.AccID == 0 {
		return fmt.Errorf("%w: place order %s has zero acc_id", domain.ErrInvalidArgument, r.ClientOrderID)
	}
	if !r.TrdEnv.IsValid() {
		return fmt.Errorf("%w: place order %s has invalid trd_env %d",
			domain.ErrInvalidArgument, r.ClientOrderID, int32(r.TrdEnv))
	}
	if r.Symbol == "" {
		return fmt.Errorf("%w: place order %s has empty symbol", domain.ErrInvalidArgument, r.ClientOrderID)
	}
	if r.Side != domain.OrderSideBuy && r.Side != domain.OrderSideSell {
		return fmt.Errorf("%w: place order %s has invalid side %q",
			domain.ErrInvalidArgument, r.ClientOrderID, r.Side)
	}
	if r.Qty <= 0 {
		return fmt.Errorf("%w: place order %s has non-positive qty %d",
			domain.ErrInvalidArgument, r.ClientOrderID, r.Qty)
	}
	return nil
}

// PlaceOrderResult is the synchronous reply to PlaceOrder: the venue order id
// for correlation. The order is NOT yet known-accepted — the executor waits for
// the Trd_UpdateOrder push (Submitted) to emit a domain ACCEPTED event,
// matching the Python adapter (which does not emit OrderAccepted on submit).
type PlaceOrderResult struct {
	VenueOrderID string
}

// OrderUpdate is one Trd_UpdateOrder push (or a Trd_GetOrderList row at
// recovery), NORMALISED away from the protobuf. DealtQty / DealtAvgPrice are
// CUMULATIVE across partial fills (Trd_Common Order.fillQty / fillAvgPrice) —
// the executor's state machine converts them to per-fill deltas. RawStatus is
// the moomoo OrderStatus int (Trd_Common); use Class() to bucket it.
// ClientOrderID is the push's correlation back to the submitting order (from
// the order remark or the client's venue->client map); it may be empty for
// external/unknown orders, which the executor drops.
type OrderUpdate struct {
	ClientOrderID string
	VenueOrderID  string
	Symbol        string
	Side          domain.OrderSide
	RawStatus     int32 // moomoo Trd_Common OrderStatus
	OrderQty      domain.Qty
	DealtQty      domain.Qty   // cumulative filled qty
	DealtAvgPrice domain.Price // cumulative average fill price
	LastErrMsg    string       // populated on reject statuses
	UpdateTimeNs  int64        // venue update time, unix nanos (0 => caller's wall clock)
}

// Class buckets the update's wire status into the lifecycle class the state
// machine switches on (reuses trd_convert.go's faithful classifier).
func (u OrderUpdate) Class() OrderStatusClass { return classifyTrdStatus(u.RawStatus) }

// IsFullFill reports the terminal FILLED_ALL status.
func (u OrderUpdate) IsFullFill() bool {
	return u.RawStatus == int32(trdcommon.OrderStatus_OrderStatus_Filled_All)
}

// IsFillCancelled reports the rare FILL_CANCELLED broker rollback status.
func (u OrderUpdate) IsFillCancelled() bool {
	return u.RawStatus == int32(trdcommon.OrderStatus_OrderStatus_FillCancelled)
}

// StatusName renders the wire status symbolically (for logs / persistence).
func (u OrderUpdate) StatusName() string { return TrdOrderStatusName(u.RawStatus) }

// FillUpdate is one Trd_UpdateOrderFill push, normalised. moomoo's fill push
// carries a PER-EXECUTION qty/price keyed by a fillID, alongside the
// OrderUpdate's cumulative view. The executor drives accounting from OrderUpdate
// cumulative deltas (the authoritative, gap-free source) and uses FillUpdate as
// a corroborating signal; carrying it keeps the seam faithful to the real
// two-stream protocol and lets recovery rebuild per-order fill state.
type FillUpdate struct {
	FillID        string
	ClientOrderID string
	VenueOrderID  string
	Symbol        string
	Side          domain.OrderSide
	Qty           domain.Qty   // per-execution qty
	Price         domain.Price // per-execution price
	UpdateTimeNs  int64
}

// Funds is the Trd_GetFunds reply, normalised: the account/buying-power view
// the pre-submit gate and the cockpit read.
type Funds struct {
	TotalAssets    domain.Money
	Cash           domain.Money
	AvailableFunds domain.Money // buying power
	MarketValue    domain.Money
}

// TradeAccount is one Trd_GetAccList row, normalised. The executor selects the
// configured acc id from this list and asserts the env matches (a live config
// must find a REAL account; a paper config a SIMULATE one).
type TradeAccount struct {
	AccID  uint64
	TrdEnv TrdEnv
	// SecurityFirm is the broker entity the account belongs to (Trd_Common
	// SecurityFirm: 1=FutuSecurities/HK, 2=FutuInc/US, 3=FutuSG, …). REAL-account
	// UnlockTrade requires it (OpenD rejects an unlock with "missing required
	// parameter securityFirm" otherwise); 0 = Unknown/unset.
	SecurityFirm int32
}

// TrdOrderHandler receives normalised Trd_UpdateOrder pushes. It is invoked
// from the client's single reader goroutine and must not block (the executor's
// handler does bounded synchronous work: state-machine step + emit + persist).
type TrdOrderHandler func(OrderUpdate)

// TrdOrderFillHandler receives normalised Trd_UpdateOrderFill pushes. Same
// threading contract as TrdOrderHandler.
type TrdOrderFillHandler func(FillUpdate)

// TradeClient is the native Trd_* trading surface. The wire client (*Client via
// trd_client.go) and the in-memory mock venue both implement it; the executor
// depends only on this interface.
//
// All money-moving methods take an explicit AccID + TrdEnv: there is no ambient
// account. This is a SAFETY property — a paper executor never holds a real acc
// id, and a live executor obtains TrdEnvReal only after the activation gate.
type TradeClient interface {
	// GetAccList returns the broker accounts for the requested env. The executor
	// uses it to bind + verify the configured acc id exists under that env.
	GetAccList(ctx context.Context, env TrdEnv) ([]TradeAccount, error)

	// UnlockTrade unlocks the REAL trading account with the password. It is a
	// no-op for the SIMULATE env. Live activation REQUIRES it to succeed before
	// any real PlaceOrder. securityFirm identifies the broker entity the target
	// account belongs to (Trd_Common SecurityFirm) — OpenD requires it on a real
	// unlock; resolve it from the account's GetAccList entry. 0 omits it (legacy
	// behaviour, for OpenD builds that don't require it).
	UnlockTrade(ctx context.Context, env TrdEnv, password string, securityFirm int32) error

	// PlaceOrder submits one order and returns the venue order id. Idempotent on
	// req.ClientOrderID (see PlaceOrderRequest doc).
	PlaceOrder(ctx context.Context, req PlaceOrderRequest) (PlaceOrderResult, error)

	// CancelOrder requests the venue cancel a working order identified by its
	// client-order-id (the order remark / idempotency key). It is used by the
	// manual trading desk to cancel a resting order before it fills. A successful
	// cancel is reported asynchronously via the normal Trd_UpdateOrder push
	// (CANCELLED_ALL), which the executor's state machine applies — this method
	// only issues the request. Cancelling an unknown, already-terminal, or
	// already-cancelled order is a no-op (nil error): cancel is idempotent so a
	// double-submit never errors. A client that cannot cancel (the market-data-
	// only surface, or a wire build without the Trd_ModifyOrder proto) returns
	// ErrUnsupported, which callers surface rather than silently dropping.
	CancelOrder(ctx context.Context, accID uint64, env TrdEnv, clientOrderID string) error

	// SubscribeOrderUpdates registers the handler for Trd_UpdateOrder pushes. The
	// executor subscribes BEFORE placing any order so it never misses the
	// SUBMITTED->FILLED transition.
	SubscribeOrderUpdates(h TrdOrderHandler) error

	// SubscribeFillUpdates registers the handler for Trd_UpdateOrderFill pushes.
	SubscribeFillUpdates(h TrdOrderFillHandler) error

	// GetOrderList returns current open/recent orders for (acc, env), for crash
	// recovery resync of in-flight order state.
	GetOrderList(ctx context.Context, accID uint64, env TrdEnv) ([]OrderUpdate, error)

	// GetPositionList returns the broker positions for (acc, env), for
	// reconciliation and crash-recovery restore.
	GetPositionList(ctx context.Context, accID uint64, env TrdEnv) ([]BrokerPosition, error)

	// GetFunds returns the account/buying-power snapshot for (acc, env).
	GetFunds(ctx context.Context, accID uint64, env TrdEnv) (Funds, error)

	// GetOrderFillList returns historical fills for (acc, env), for recovery to
	// rebuild per-order cumulative fill state.
	GetOrderFillList(ctx context.Context, accID uint64, env TrdEnv) ([]FillUpdate, error)
}
