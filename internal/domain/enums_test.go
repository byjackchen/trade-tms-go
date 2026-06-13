package domain

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestEnumValues(t *testing.T) {
	// Exact wire values [MUST-MATCH the Python StrEnums/literals].
	checks := []struct {
		got, want string
	}{
		{string(SideLong), "LONG"}, {string(SideFlat), "FLAT"}, {string(SideShort), "SHORT"},
		{string(StateNoSetup), "no_setup"}, {string(StateForming), "forming"},
		{string(StateBuy), "buy"}, {string(StateHold), "hold"},
		{string(StateExit), "exit"}, {string(StateStopHit), "stop_hit"},
		{string(GradeAPlus), "A+"}, {string(GradeB), "B"}, {string(GradeSkip), "skip"},
		{string(RegimeBull), "bull"}, {string(RegimeBear), "bear"},
		{string(RegimeNeutral), "neutral"}, {string(RegimeWarning), "warning"},
		{string(SessionPre), "pre"}, {string(SessionRegular), "regular"},
		{string(SessionPost), "post"}, {string(SessionClosed), "closed"},
		{string(LegLong), "long"}, {string(LegShort), "short"},
		{string(ModeSignal), "signal"}, {string(ModePaper), "paper"}, {string(ModeLive), "live"},
		{string(OrderSideBuy), "BUY"}, {string(OrderSideSell), "SELL"},
		{string(OrderTypeMarket), "MARKET"}, {string(TIFGTC), "GTC"},
		{StrategyIDSEPA, "sepa"}, {StrategyIDPairs, "pairs"},
		{StrategyIDSectorRotation, "sector_rotation"}, {StrategyIDIntradayBreakout, "intraday_breakout"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("enum value %q, want %q", c.got, c.want)
		}
	}
}

func TestEnumParseValidate(t *testing.T) {
	if v, err := ParseSignalSide("LONG"); err != nil || v != SideLong {
		t.Errorf("ParseSignalSide: %v, %v", v, err)
	}
	if _, err := ParseSignalSide("long"); !errors.Is(err, ErrInvalidArgument) {
		t.Error("SignalSide is case-sensitive (StrEnum parity)")
	}
	if _, err := ParseSignalSide(""); err == nil {
		t.Error("empty SignalSide must be rejected")
	}
	if SignalSide("BOGUS").IsValid() {
		t.Error("BOGUS must not validate")
	}

	if v, err := ParseOrderSide("SELL"); err != nil || v != OrderSideSell {
		t.Errorf("ParseOrderSide: %v, %v", v, err)
	}
	if _, err := ParseOrderType("MARKET"); err != nil {
		t.Errorf("ParseOrderType: %v", err)
	}
	if _, err := ParseOrderType("ICEBERG"); err == nil {
		t.Error("unknown OrderType must be rejected")
	}
	if v, err := ParseTimeInForce("GTC"); err != nil || v != TIFGTC {
		t.Errorf("ParseTimeInForce: %v, %v", v, err)
	}
	if v, err := ParseMode("paper"); err != nil || v != ModePaper {
		t.Errorf("ParseMode: %v, %v", v, err)
	}
	if _, err := ParseMode("backtest"); err == nil {
		t.Error("unknown Mode must be rejected")
	}
	if v, err := ParseSignalState("stop_hit"); err != nil || v != StateStopHit {
		t.Errorf("ParseSignalState: %v, %v", v, err)
	}
	if _, err := ParseSignalState("STOP_HIT"); err == nil {
		t.Error("SignalState is lowercase (StrEnum value parity)")
	}
	if v, err := ParseOrderStatus("PARTIALLY_FILLED"); err != nil || v != OrderStatusPartiallyFilled {
		t.Errorf("ParseOrderStatus: %v, %v", v, err)
	}
	if v, err := ParseRegime("warning"); err != nil || v != RegimeWarning {
		t.Errorf("ParseRegime: %v, %v", v, err)
	}
	if v, err := ParseMarketSession("pre"); err != nil || v != SessionPre {
		t.Errorf("ParseMarketSession: %v, %v", v, err)
	}
	if v, err := ParseLegRole("short"); err != nil || v != LegShort {
		t.Errorf("ParseLegRole: %v, %v", v, err)
	}
	if v, err := ParseGrade("A+"); err != nil || v != GradeAPlus {
		t.Errorf("ParseGrade: %v, %v", v, err)
	}
	if _, err := ParseGrade("C"); err == nil {
		t.Error("unknown Grade must be rejected")
	}
}

func TestEnumJSON(t *testing.T) {
	type wrapper struct {
		Side  SignalSide  `json:"side"`
		State SignalState `json:"state"`
		Mode  Mode        `json:"mode"`
	}
	w := wrapper{Side: SideShort, State: StateForming, Mode: ModeLive}
	b, err := json.Marshal(w)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"side":"SHORT","state":"forming","mode":"live"}`
	if string(b) != want {
		t.Errorf("marshal = %s, want %s", b, want)
	}
	var back wrapper
	if err := json.Unmarshal(b, &back); err != nil || back != w {
		t.Errorf("round trip = %+v, %v", back, err)
	}
	// Unknown values are rejected at decode time.
	if err := json.Unmarshal([]byte(`{"side":"SIDEWAYS"}`), &back); err == nil {
		t.Error("decoding an unknown enum value must fail")
	}
	if err := json.Unmarshal([]byte(`{"mode":"production"}`), &back); err == nil {
		t.Error("decoding an unknown Mode must fail")
	}
}

func TestOrderSideMapping(t *testing.T) {
	if v, err := OrderSideFor(SideLong); err != nil || v != OrderSideBuy {
		t.Errorf("OrderSideFor(LONG) = %v, %v", v, err)
	}
	if v, err := OrderSideFor(SideShort); err != nil || v != OrderSideSell {
		t.Errorf("OrderSideFor(SHORT) = %v, %v", v, err)
	}
	if _, err := OrderSideFor(SideFlat); !errors.Is(err, ErrInvalidArgument) {
		t.Error("OrderSideFor(FLAT) must error (depends on net position)")
	}
	if _, err := OrderSideFor(SignalSide("x")); err == nil {
		t.Error("OrderSideFor must reject invalid side")
	}
}

func TestCloseSideFor(t *testing.T) {
	// FLAT translation [MUST-MATCH §7.4]: SELL if net>0, BUY if net<0,
	// no order when net == 0.
	tests := []struct {
		net  Qty
		side OrderSide
		ok   bool
	}{
		{100, OrderSideSell, true},
		{-100, OrderSideBuy, true},
		{1, OrderSideSell, true},
		{-1, OrderSideBuy, true},
		{0, "", false},
	}
	for _, tt := range tests {
		side, ok := CloseSideFor(tt.net)
		if ok != tt.ok || side != tt.side {
			t.Errorf("CloseSideFor(%d) = %q, %v; want %q, %v", tt.net, side, ok, tt.side, tt.ok)
		}
	}
}

func TestOrderStatusTerminal(t *testing.T) {
	terminal := map[OrderStatus]bool{
		OrderStatusSubmitted: false, OrderStatusAccepted: false,
		OrderStatusPartiallyFilled: false, OrderStatusFilled: true,
		OrderStatusCanceled: true, OrderStatusRejected: true,
	}
	for s, want := range terminal {
		if s.IsTerminal() != want {
			t.Errorf("%s.IsTerminal() = %v, want %v", s, s.IsTerminal(), want)
		}
	}
}
