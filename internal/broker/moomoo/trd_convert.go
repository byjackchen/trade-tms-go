package moomoo

// trd_convert.go defines the trading value objects + enums shared across the
// Trd_* seam (TrdEnv, BrokerPosition, OrderStatusClass) and the FAITHFUL mapping
// between moomoo's Trd_* wire representation and the project's domain trading
// types. trade.go (the executor-facing contract) and trd_client.go (the wire
// client) both build on these.
//
// The order-status classification sorts every moomoo OrderStatus into dispatch
// buckets (accept / fill / cancel / reject / transient). Keeping the buckets
// stable across the mock and a real account is what makes "green on the mock"
// predict "green on a real account": the SAME moomoo status drives the SAME
// lifecycle transition in both.
//
// AUTHORITATIVE: Trd_Common.proto (TrdEnv / TrdSide / OrderType / OrderStatus /
// PositionSide enums).

import (
	"fmt"
	"strings"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/broker/moomoo/pb/trdcommon"
	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// TrdEnv selects the broker environment, mirroring moomoo's TrdEnv enum
// (Trd_Common.proto: TrdEnv_Simulate=0, TrdEnv_Real=1). The zero value is
// Simulate (paper) — the SAFE default: a misconfiguration can only fall back to
// the paper account, never to real money.
type TrdEnv int32

const (
	// TrdEnvSimulate is the moomoo paper / simulated account.
	TrdEnvSimulate TrdEnv = TrdEnv(trdcommon.TrdEnv_TrdEnv_Simulate) // 0
	// TrdEnvReal is the moomoo REAL-money account. Reaching this value requires
	// the full live-activation gate (see internal/exec/moomoo).
	TrdEnvReal TrdEnv = TrdEnv(trdcommon.TrdEnv_TrdEnv_Real) // 1
)

// IsValid reports whether e is a known TrdEnv.
func (e TrdEnv) IsValid() bool { return e == TrdEnvSimulate || e == TrdEnvReal }

// IsReal reports whether e targets the real-money account.
func (e TrdEnv) IsReal() bool { return e == TrdEnvReal }

// String renders the env for logs.
func (e TrdEnv) String() string {
	switch e {
	case TrdEnvSimulate:
		return "SIMULATE"
	case TrdEnvReal:
		return "REAL"
	default:
		return fmt.Sprintf("TrdEnv(%d)", int32(e))
	}
}

// TrdMarketUS is the US trading market — the only market in the project
// universe. Exposed so the client and the mock venue agree.
const TrdMarketUS = int32(trdcommon.TrdMarket_TrdMarket_US)

// TrdSecMarketUS is the US security-market qualifier carried on PlaceOrder.
const TrdSecMarketUS = int32(trdcommon.TrdSecMarket_TrdSecMarket_US)

// trdSideForDomain maps a domain OrderSide to the moomoo TrdSide. The client
// only ever SUBMITS Buy/Sell; SellShort/BuyBack are values the SERVER may return
// (see sideFromTrd).
func trdSideForDomain(side domain.OrderSide) (int32, error) {
	switch side {
	case domain.OrderSideBuy:
		return int32(trdcommon.TrdSide_TrdSide_Buy), nil
	case domain.OrderSideSell:
		return int32(trdcommon.TrdSide_TrdSide_Sell), nil
	default:
		return 0, fmt.Errorf("%w: cannot map order side %q to TrdSide", domain.ErrInvalidArgument, side)
	}
}

// sideFromTrd maps a moomoo TrdSide back to a domain OrderSide. Buy/BuyBack ->
// BUY; Sell/SellShort -> SELL (a US short is reported by the server as
// SellShort; we collapse it to SELL, matching the netting model).
func sideFromTrd(trdSide int32) (domain.OrderSide, error) {
	switch trdcommon.TrdSide(trdSide) {
	case trdcommon.TrdSide_TrdSide_Buy, trdcommon.TrdSide_TrdSide_BuyBack:
		return domain.OrderSideBuy, nil
	case trdcommon.TrdSide_TrdSide_Sell, trdcommon.TrdSide_TrdSide_SellShort:
		return domain.OrderSideSell, nil
	default:
		return "", fmt.Errorf("%w: unknown TrdSide %d", domain.ErrInvalidArgument, trdSide)
	}
}

// OrderStatusClass is the coarse order-lifecycle bucket the executor's state
// machine reacts to. It abstracts moomoo's ~16 fine-grained Trd_Common
// OrderStatus values into the handful the engine + accounting + DB care about.
type OrderStatusClass int

const (
	// StatusClassTransient: in-flight, no domain-visible transition yet
	// (Submitting / WaitingSubmit / Cancelling_*). The next push delivers an
	// accepted/terminal state; the executor emits nothing.
	StatusClassTransient OrderStatusClass = iota
	// StatusClassAccepted: working at the venue (moomoo Submitted).
	StatusClassAccepted
	// StatusClassFilled: partially or fully filled (Filled_Part / Filled_All).
	StatusClassFilled
	// StatusClassCanceled: terminal cancel (Cancelled_*/Deleted/FillCancelled).
	StatusClassCanceled
	// StatusClassRejected: terminal reject (Failed/Disabled/SubmitFailed/TimeOut).
	StatusClassRejected
	// StatusClassUnknown: a status outside every known set — a state-drift risk
	// the caller must log at WARN and surface.
	StatusClassUnknown
)

// String renders the class for logs.
func (c OrderStatusClass) String() string {
	switch c {
	case StatusClassTransient:
		return "TRANSIENT"
	case StatusClassAccepted:
		return "ACCEPTED"
	case StatusClassFilled:
		return "FILLED"
	case StatusClassCanceled:
		return "CANCELED"
	case StatusClassRejected:
		return "REJECTED"
	default:
		return "UNKNOWN"
	}
}

// IsTerminal reports whether the class is a final state (no further
// transitions): FILLED is terminal only on FILLED_ALL — for a partial the caller
// inspects the raw status; here FILLED covers both partial and full, so
// IsTerminal is false for it. CANCELED / REJECTED are terminal.
func (c OrderStatusClass) IsTerminal() bool {
	switch c {
	case StatusClassCanceled, StatusClassRejected:
		return true
	}
	return false
}

// classifyTrdStatus buckets a raw moomoo Trd_Common.OrderStatus int. It is the
// single authority for moomoo->lifecycle mapping; the mock venue and the wire
// client both go through it, so they cannot drift.
//
// Bucket definitions:
//   - accept    = {SUBMITTED}
//   - fill      = {FILLED_PART, FILLED_ALL}
//   - cancel    = {CANCELLED_PART, CANCELLED_ALL, DELETED, FILL_CANCELLED}
//   - reject    = {FAILED, DISABLED, SUBMIT_FAILED, TIMEOUT}
//   - transient = {SUBMITTING, WAITING_SUBMIT, CANCELLING_PART, CANCELLING_ALL}
func classifyTrdStatus(raw int32) OrderStatusClass {
	switch trdcommon.OrderStatus(raw) {
	case trdcommon.OrderStatus_OrderStatus_Submitted:
		return StatusClassAccepted
	case trdcommon.OrderStatus_OrderStatus_Filled_Part,
		trdcommon.OrderStatus_OrderStatus_Filled_All:
		return StatusClassFilled
	case trdcommon.OrderStatus_OrderStatus_Cancelled_Part,
		trdcommon.OrderStatus_OrderStatus_Cancelled_All,
		trdcommon.OrderStatus_OrderStatus_Deleted,
		trdcommon.OrderStatus_OrderStatus_FillCancelled:
		return StatusClassCanceled
	case trdcommon.OrderStatus_OrderStatus_Failed,
		trdcommon.OrderStatus_OrderStatus_Disabled,
		trdcommon.OrderStatus_OrderStatus_SubmitFailed,
		trdcommon.OrderStatus_OrderStatus_TimeOut:
		return StatusClassRejected
	case trdcommon.OrderStatus_OrderStatus_Submitting,
		trdcommon.OrderStatus_OrderStatus_WaitingSubmit,
		trdcommon.OrderStatus_OrderStatus_Cancelling_Part,
		trdcommon.OrderStatus_OrderStatus_Cancelling_All:
		return StatusClassTransient
	default:
		return StatusClassUnknown
	}
}

// DomainOrderStatus maps a raw moomoo OrderStatus to the project's
// domain.OrderStatus where one exists. Filled_All -> FILLED; Filled_Part ->
// PARTIALLY_FILLED; the cancel/reject buckets to their terminal domain states;
// Submitted -> ACCEPTED. Transient/unknown raw statuses have no single domain
// status and return ok=false (the caller keeps the order's prior status).
func DomainOrderStatus(raw int32) (domain.OrderStatus, bool) {
	switch classifyTrdStatus(raw) {
	case StatusClassAccepted:
		return domain.OrderStatusAccepted, true
	case StatusClassFilled:
		if trdcommon.OrderStatus(raw) == trdcommon.OrderStatus_OrderStatus_Filled_All {
			return domain.OrderStatusFilled, true
		}
		return domain.OrderStatusPartiallyFilled, true
	case StatusClassCanceled:
		return domain.OrderStatusCanceled, true
	case StatusClassRejected:
		return domain.OrderStatusRejected, true
	default:
		return "", false
	}
}

// TrdOrderStatusName returns the symbolic moomoo OrderStatus name (e.g.
// "Filled_All") for logs, persistence, and reconciliation reports.
func TrdOrderStatusName(raw int32) string {
	return strings.TrimPrefix(trdcommon.OrderStatus(raw).String(), "OrderStatus_OrderStatus_")
}

// BrokerPosition is one Trd_GetPositionList row, normalised to the project's
// signed-qty convention (positive long, negative short), for reconciliation and
// crash-recovery restore.
type BrokerPosition struct {
	Symbol    string
	Qty       domain.Qty // signed: long > 0, short < 0
	AvgPrice  domain.Price
	CostPrice domain.Price
	Price     domain.Price // last market price
	PLVal     domain.Money // unrealized P&L
}

// brokerPositionFromTrd converts a moomoo Trd_Common.Position. moomoo reports a
// non-negative qty with a separate positionSide; we fold side into the sign.
func brokerPositionFromTrd(p *trdcommon.Position) (BrokerPosition, error) {
	if p == nil {
		return BrokerPosition{}, fmt.Errorf("%w: nil broker position", domain.ErrInvalidArgument)
	}
	qty, err := domain.QtyFromFloat64Trunc(p.GetQty())
	if err != nil {
		return BrokerPosition{}, fmt.Errorf("moomoo: broker position %s qty: %w", p.GetCode(), err)
	}
	if trdcommon.PositionSide(p.GetPositionSide()) == trdcommon.PositionSide_PositionSide_Short {
		qty = -qty
	}
	avgF := p.GetAverageCostPrice()
	if avgF == 0 {
		avgF = p.GetCostPrice() // fall back to the deprecated field for sim accounts
	}
	avg, _ := domain.PriceFromFloat64(avgF)
	cost, _ := domain.PriceFromFloat64(p.GetCostPrice())
	last, _ := domain.PriceFromFloat64(p.GetPrice())
	pl, _ := domain.MoneyFromFloat64(p.GetPlVal())
	return BrokerPosition{
		Symbol:    p.GetCode(),
		Qty:       qty,
		AvgPrice:  avg,
		CostPrice: cost,
		Price:     last,
		PLVal:     pl,
	}, nil
}

// trdTimeLayouts are the moomoo Trd_* createTime/updateTime string formats
// ("YYYY-MM-DD HH:MM:SS" or with ".MS"), interpreted in the exchange (NY) zone
// like the K-line strings. The numeric *Timestamp epoch fields are preferred
// when present (unambiguous, UTC).
var trdTimeLayouts = []string{
	"2006-01-02 15:04:05.999999999",
	"2006-01-02 15:04:05",
}

// parseTrdTime parses a moomoo Trd time string (NY-local) to UTC.
func parseTrdTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, fmt.Errorf("%w: empty Trd time string", domain.ErrInvalidArgument)
	}
	var firstErr error
	for _, layout := range trdTimeLayouts {
		t, err := time.ParseInLocation(layout, s, nyLoc)
		if err == nil {
			return t.UTC(), nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return time.Time{}, fmt.Errorf("moomoo: parse Trd time %q: %w", s, firstErr)
}

// trdInstant prefers the numeric epoch-seconds field; on zero/absent it falls
// back to parsing the NY-local time string. Either way it returns UTC.
func trdInstant(epochSec float64, timeStr string) (time.Time, error) {
	if epochSec != 0 {
		whole := int64(epochSec)
		nanos := int64((epochSec - float64(whole)) * 1e9)
		return time.Unix(whole, nanos).UTC(), nil
	}
	return parseTrdTime(timeStr)
}

// FormatTrdTime renders a UTC instant to moomoo's NY-local Trd time string. Used
// by the mock trading venue to stamp createTime/updateTime on orders/fills.
func FormatTrdTime(tsUTC time.Time) string {
	return tsUTC.In(nyLoc).Format("2006-01-02 15:04:05")
}
