package domain

// enums.go defines the string-valued enumerations shared across the system.
// Values are MUST-MATCH copies of the Python reference StrEnums / literals
// (docs/spec/domain-types-money.md §2.1, §2.4, §2.6, §2.15, §7.6): the enum
// value IS the wire/log string. Every enum validates on parse and on JSON
// decode (via encoding.TextUnmarshaler) and rejects unknown values.

import "fmt"

// ---------------------------------------------------------------------------
// SignalSide — src/strategies/sepa/signal.py:62-67 [MUST-MATCH]
// ---------------------------------------------------------------------------

// SignalSide is the strategy-level direction shared by all four strategies
// and by ProposedOrder. LONG maps to broker BUY, SHORT to SELL (margin only),
// FLAT means close-everything.
type SignalSide string

const (
	SideLong  SignalSide = "LONG"
	SideFlat  SignalSide = "FLAT"
	SideShort SignalSide = "SHORT"
)

// IsValid reports whether s is a known SignalSide.
func (s SignalSide) IsValid() bool {
	switch s {
	case SideLong, SideFlat, SideShort:
		return true
	}
	return false
}

// String returns the exact Python StrEnum value.
func (s SignalSide) String() string { return string(s) }

// ParseSignalSide validates and returns the SignalSide for s.
func ParseSignalSide(s string) (SignalSide, error) {
	v := SignalSide(s)
	if !v.IsValid() {
		return "", fmt.Errorf("%w: unknown SignalSide %q", ErrInvalidArgument, s)
	}
	return v, nil
}

// MarshalText implements encoding.TextMarshaler.
func (s SignalSide) MarshalText() ([]byte, error) { return []byte(s), nil }

// UnmarshalText implements encoding.TextUnmarshaler with validation.
func (s *SignalSide) UnmarshalText(b []byte) error {
	v, err := ParseSignalSide(string(b))
	if err != nil {
		return err
	}
	*s = v
	return nil
}

// ---------------------------------------------------------------------------
// OrderSide — broker-level direction (Nautilus OrderSide names)
// ---------------------------------------------------------------------------

// OrderSide is the broker order direction. SignalSide LONG → BUY,
// SHORT → SELL; FLAT is translated per the live net position (CloseSideFor).
type OrderSide string

const (
	OrderSideBuy  OrderSide = "BUY"
	OrderSideSell OrderSide = "SELL"
)

// IsValid reports whether o is a known OrderSide.
func (o OrderSide) IsValid() bool { return o == OrderSideBuy || o == OrderSideSell }

// String returns the wire value.
func (o OrderSide) String() string { return string(o) }

// ParseOrderSide validates and returns the OrderSide for s.
func ParseOrderSide(s string) (OrderSide, error) {
	v := OrderSide(s)
	if !v.IsValid() {
		return "", fmt.Errorf("%w: unknown OrderSide %q", ErrInvalidArgument, s)
	}
	return v, nil
}

// MarshalText implements encoding.TextMarshaler.
func (o OrderSide) MarshalText() ([]byte, error) { return []byte(o), nil }

// UnmarshalText implements encoding.TextUnmarshaler with validation.
func (o *OrderSide) UnmarshalText(b []byte) error {
	v, err := ParseOrderSide(string(b))
	if err != nil {
		return err
	}
	*o = v
	return nil
}

// OrderSideFor maps an entry SignalSide to its broker side. FLAT has no
// static mapping (it depends on the net position — use CloseSideFor) and
// returns an error, as does any invalid side.
func OrderSideFor(s SignalSide) (OrderSide, error) {
	switch s {
	case SideLong:
		return OrderSideBuy, nil
	case SideShort:
		return OrderSideSell, nil
	default:
		return "", fmt.Errorf("%w: no static OrderSide for SignalSide %q", ErrInvalidArgument, s)
	}
}

// CloseSideFor returns the order side that flattens a net position per the
// reference FLAT translation (§7.4): SELL when net > 0, BUY when net < 0.
// ok is false when net == 0 (no order is emitted).
func CloseSideFor(net Qty) (side OrderSide, ok bool) {
	switch {
	case net > 0:
		return OrderSideSell, true
	case net < 0:
		return OrderSideBuy, true
	default:
		return "", false
	}
}

// ---------------------------------------------------------------------------
// OrderType — §7.6: the reference system uses MARKET orders exclusively
// ---------------------------------------------------------------------------

// OrderType is the order kind. The reference system submits MARKET orders
// only (stops are strategy-evaluated, §7.6); the remaining kinds are defined
// for forward compatibility and validate like any other value.
type OrderType string

const (
	OrderTypeMarket     OrderType = "MARKET"
	OrderTypeLimit      OrderType = "LIMIT"
	OrderTypeStopMarket OrderType = "STOP_MARKET"
	OrderTypeStopLimit  OrderType = "STOP_LIMIT"
)

// IsValid reports whether t is a known OrderType.
func (t OrderType) IsValid() bool {
	switch t {
	case OrderTypeMarket, OrderTypeLimit, OrderTypeStopMarket, OrderTypeStopLimit:
		return true
	}
	return false
}

// String returns the wire value.
func (t OrderType) String() string { return string(t) }

// ParseOrderType validates and returns the OrderType for s.
func ParseOrderType(s string) (OrderType, error) {
	v := OrderType(s)
	if !v.IsValid() {
		return "", fmt.Errorf("%w: unknown OrderType %q", ErrInvalidArgument, s)
	}
	return v, nil
}

// MarshalText implements encoding.TextMarshaler.
func (t OrderType) MarshalText() ([]byte, error) { return []byte(t), nil }

// UnmarshalText implements encoding.TextUnmarshaler with validation.
func (t *OrderType) UnmarshalText(b []byte) error {
	v, err := ParseOrderType(string(b))
	if err != nil {
		return err
	}
	*t = v
	return nil
}

// ---------------------------------------------------------------------------
// TimeInForce — §7.6: all reference orders are GTC
// ---------------------------------------------------------------------------

// TimeInForce is the order lifetime policy. The reference system uses GTC
// for every order.
type TimeInForce string

const (
	TIFGTC TimeInForce = "GTC"
	TIFDay TimeInForce = "DAY"
	TIFIOC TimeInForce = "IOC"
	TIFFOK TimeInForce = "FOK"
)

// IsValid reports whether t is a known TimeInForce.
func (t TimeInForce) IsValid() bool {
	switch t {
	case TIFGTC, TIFDay, TIFIOC, TIFFOK:
		return true
	}
	return false
}

// String returns the wire value.
func (t TimeInForce) String() string { return string(t) }

// ParseTimeInForce validates and returns the TimeInForce for s.
func ParseTimeInForce(s string) (TimeInForce, error) {
	v := TimeInForce(s)
	if !v.IsValid() {
		return "", fmt.Errorf("%w: unknown TimeInForce %q", ErrInvalidArgument, s)
	}
	return v, nil
}

// MarshalText implements encoding.TextMarshaler.
func (t TimeInForce) MarshalText() ([]byte, error) { return []byte(t), nil }

// UnmarshalText implements encoding.TextUnmarshaler with validation.
func (t *TimeInForce) UnmarshalText(b []byte) error {
	v, err := ParseTimeInForce(string(b))
	if err != nil {
		return err
	}
	*t = v
	return nil
}

// ---------------------------------------------------------------------------
// OrderStatus — order lifecycle states
// ---------------------------------------------------------------------------

// OrderStatus is the lifecycle state of an Order snapshot.
type OrderStatus string

const (
	OrderStatusSubmitted       OrderStatus = "SUBMITTED"
	OrderStatusAccepted        OrderStatus = "ACCEPTED"
	OrderStatusPartiallyFilled OrderStatus = "PARTIALLY_FILLED"
	OrderStatusFilled          OrderStatus = "FILLED"
	OrderStatusCanceled        OrderStatus = "CANCELED"
	OrderStatusRejected        OrderStatus = "REJECTED"
)

// IsValid reports whether s is a known OrderStatus.
func (s OrderStatus) IsValid() bool {
	switch s {
	case OrderStatusSubmitted, OrderStatusAccepted, OrderStatusPartiallyFilled,
		OrderStatusFilled, OrderStatusCanceled, OrderStatusRejected:
		return true
	}
	return false
}

// String returns the wire value.
func (s OrderStatus) String() string { return string(s) }

// ParseOrderStatus validates and returns the OrderStatus for s.
func ParseOrderStatus(s string) (OrderStatus, error) {
	v := OrderStatus(s)
	if !v.IsValid() {
		return "", fmt.Errorf("%w: unknown OrderStatus %q", ErrInvalidArgument, s)
	}
	return v, nil
}

// MarshalText implements encoding.TextMarshaler.
func (s OrderStatus) MarshalText() ([]byte, error) { return []byte(s), nil }

// UnmarshalText implements encoding.TextUnmarshaler with validation.
func (s *OrderStatus) UnmarshalText(b []byte) error {
	v, err := ParseOrderStatus(string(b))
	if err != nil {
		return err
	}
	*s = v
	return nil
}

// IsTerminal reports whether the status is final (no further transitions).
func (s OrderStatus) IsTerminal() bool {
	switch s {
	case OrderStatusFilled, OrderStatusCanceled, OrderStatusRejected:
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// SignalState — shared intent state machine [MUST-MATCH]
// (src/strategies/*/intent.py, identical in all four strategies)
// ---------------------------------------------------------------------------

// SignalState is the SignalIntent state machine value.
type SignalState string

const (
	StateNoSetup SignalState = "no_setup"
	StateForming SignalState = "forming"
	StateBuy     SignalState = "buy"
	StateHold    SignalState = "hold"
	StateExit    SignalState = "exit"
	StateStopHit SignalState = "stop_hit"
)

// IsValid reports whether s is a known SignalState.
func (s SignalState) IsValid() bool {
	switch s {
	case StateNoSetup, StateForming, StateBuy, StateHold, StateExit, StateStopHit:
		return true
	}
	return false
}

// String returns the exact Python StrEnum value.
func (s SignalState) String() string { return string(s) }

// ParseSignalState validates and returns the SignalState for s.
func ParseSignalState(s string) (SignalState, error) {
	v := SignalState(s)
	if !v.IsValid() {
		return "", fmt.Errorf("%w: unknown SignalState %q", ErrInvalidArgument, s)
	}
	return v, nil
}

// MarshalText implements encoding.TextMarshaler.
func (s SignalState) MarshalText() ([]byte, error) { return []byte(s), nil }

// UnmarshalText implements encoding.TextUnmarshaler with validation.
func (s *SignalState) UnmarshalText(b []byte) error {
	v, err := ParseSignalState(string(b))
	if err != nil {
		return err
	}
	*s = v
	return nil
}

// ---------------------------------------------------------------------------
// Regime — market regime values [MUST-MATCH] (src/data/custom_data.py:27-42)
// ---------------------------------------------------------------------------

// Regime is the market regime published via RegimeUpdate.
type Regime string

const (
	RegimeBull    Regime = "bull"
	RegimeBear    Regime = "bear"
	RegimeNeutral Regime = "neutral"
	RegimeWarning Regime = "warning"
)

// IsValid reports whether r is a known Regime.
func (r Regime) IsValid() bool {
	switch r {
	case RegimeBull, RegimeBear, RegimeNeutral, RegimeWarning:
		return true
	}
	return false
}

// String returns the wire value.
func (r Regime) String() string { return string(r) }

// ParseRegime validates and returns the Regime for s.
func ParseRegime(s string) (Regime, error) {
	v := Regime(s)
	if !v.IsValid() {
		return "", fmt.Errorf("%w: unknown Regime %q", ErrInvalidArgument, s)
	}
	return v, nil
}

// MarshalText implements encoding.TextMarshaler.
func (r Regime) MarshalText() ([]byte, error) { return []byte(r), nil }

// UnmarshalText implements encoding.TextUnmarshaler with validation.
func (r *Regime) UnmarshalText(b []byte) error {
	v, err := ParseRegime(string(b))
	if err != nil {
		return err
	}
	*r = v
	return nil
}

// ---------------------------------------------------------------------------
// MarketSession — QuoteUpdate session values [MUST-MATCH]
// (src/data/custom_data.py:215-238)
// ---------------------------------------------------------------------------

// MarketSession is the trading-session phase carried by quote updates.
type MarketSession string

const (
	SessionPre     MarketSession = "pre"
	SessionRegular MarketSession = "regular"
	SessionPost    MarketSession = "post"
	SessionClosed  MarketSession = "closed"
)

// IsValid reports whether s is a known MarketSession.
func (s MarketSession) IsValid() bool {
	switch s {
	case SessionPre, SessionRegular, SessionPost, SessionClosed:
		return true
	}
	return false
}

// String returns the wire value.
func (s MarketSession) String() string { return string(s) }

// ParseMarketSession validates and returns the MarketSession for s.
func ParseMarketSession(s string) (MarketSession, error) {
	v := MarketSession(s)
	if !v.IsValid() {
		return "", fmt.Errorf("%w: unknown MarketSession %q", ErrInvalidArgument, s)
	}
	return v, nil
}

// MarshalText implements encoding.TextMarshaler.
func (s MarketSession) MarshalText() ([]byte, error) { return []byte(s), nil }

// UnmarshalText implements encoding.TextUnmarshaler with validation.
func (s *MarketSession) UnmarshalText(b []byte) error {
	v, err := ParseMarketSession(string(b))
	if err != nil {
		return err
	}
	*s = v
	return nil
}

// ---------------------------------------------------------------------------
// LegRole — pairs leg role [MUST-MATCH] (src/strategies/pairs/intent.py)
// ---------------------------------------------------------------------------

// LegRole identifies which leg of a pair an intent describes.
type LegRole string

const (
	LegLong  LegRole = "long"
	LegShort LegRole = "short"
)

// IsValid reports whether l is a known LegRole.
func (l LegRole) IsValid() bool { return l == LegLong || l == LegShort }

// String returns the wire value.
func (l LegRole) String() string { return string(l) }

// ParseLegRole validates and returns the LegRole for s.
func ParseLegRole(s string) (LegRole, error) {
	v := LegRole(s)
	if !v.IsValid() {
		return "", fmt.Errorf("%w: unknown LegRole %q", ErrInvalidArgument, s)
	}
	return v, nil
}

// MarshalText implements encoding.TextMarshaler.
func (l LegRole) MarshalText() ([]byte, error) { return []byte(l), nil }

// UnmarshalText implements encoding.TextUnmarshaler with validation.
func (l *LegRole) UnmarshalText(b []byte) error {
	v, err := ParseLegRole(string(b))
	if err != nil {
		return err
	}
	*l = v
	return nil
}
