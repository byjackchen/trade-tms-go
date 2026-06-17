package sepaadapter

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/engine"
	"github.com/byjackchen/trade-tms-go/internal/strategy/sepa"
)

// fakeSub records submitted orders and answers net-position reads.
type fakeSub struct {
	orders []order
	net    domain.Qty
}

type order struct {
	side domain.OrderSide
	qty  domain.Qty
}

func (f *fakeSub) SubmitMarket(_, _ string, side domain.OrderSide, qty domain.Qty, _ string, _ time.Time) (string, error) {
	f.orders = append(f.orders, order{side, qty})
	return "coid", nil
}
func (f *fakeSub) SubmitMarketSignal(id, symbol string, _ domain.SignalSide, side domain.OrderSide, qty domain.Qty, reason string, ts time.Time) (string, bool, error) {
	coid, err := f.SubmitMarket(id, symbol, side, qty, reason, ts)
	return coid, err == nil, err
}
func (f *fakeSub) NetPosition(_, _ string) domain.Qty { return f.net }

func newGen(t *testing.T, symbol string) *sepa.Generator {
	t.Helper()
	g, err := sepa.New(sepa.Config{
		Symbol: symbol, EquityProvider: func() float64 { return 100000 },
		RiskPct: 1.0, MarketCapMinUSD: 5e8, HardStopPct: 7.5, PivotBufferPct: 1.5,
		BreakoutVolumeMultiple: 1.5, VCPLookback: 4, HistoryMaxBars: 1000, Timezone: "America/New_York",
	})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// newAdapter constructs a sepaadapter.Strategy, failing the test on error.
func newAdapter(t *testing.T, id string, gen *sepa.Generator) *Strategy {
	t.Helper()
	s, err := New(id, gen)
	if err != nil {
		t.Fatalf("sepaadapter.New: %v", err)
	}
	return s
}

func mkBar(sym string, ts time.Time, c float64, v int64) domain.Bar {
	p := func(f float64) domain.Price {
		x, _ := domain.PriceFromFloat64(f)
		return x
	}
	return domain.Bar{Symbol: sym, TS: ts, Open: p(c), High: p(c + 0.5), Low: p(c - 0.5), Close: p(c), Volume: v}
}

func TestAdapterImplementsCapabilities(t *testing.T) {
	var s engine.Strategy = newAdapter(t, "SEPA-AAPL", newGen(t, "AAPL"))
	if _, ok := s.(engine.ContextConsumer); !ok {
		t.Error("not a ContextConsumer")
	}
	if _, ok := s.(engine.SignalEvaluator); !ok {
		t.Error("not an SignalEvaluator")
	}
	if _, ok := s.(engine.StateSummarizer); !ok {
		t.Error("not a StateSummarizer")
	}
	if _, ok := s.(engine.StatePersister); !ok {
		t.Error("not a StatePersister")
	}
}

func TestInjectContextRoutesBySymbol(t *testing.T) {
	s := newAdapter(t, "SEPA-AAPL", newGen(t, "AAPL"))
	s.InjectContext(engine.StrategyContext{
		Regime:           "bull",
		MarketCapUSD:     map[string]float64{"AAPL": 9e9, "MSFT": 1e9},
		EarningsBlackout: map[string]bool{"AAPL": true},
	})
	// Reflected through state summary.
	js := s.StateSummaryJSON().(summaryJSON)
	if js.Regime != "bull" || js.MarketCapUSD != 9e9 || !js.InBlackout {
		t.Fatalf("context not routed: %+v", js)
	}
}

func TestFlatSignalClosesNetPosition(t *testing.T) {
	// Manually drive a generator into a held state via a tiny synthetic path is
	// heavy; instead verify the FLAT translation directly through a forced exit
	// by priming a position then feeding a sub-stop bar is also heavy. Here we
	// assert the FLAT path reverses a long net into a SELL of equal magnitude.
	g := newGen(t, "AAPL")
	s := newAdapter(t, "SEPA-AAPL", g)
	sub := &fakeSub{net: 100}
	// Emit a FLAT manually by exercising submit via a crafted signal is internal;
	// instead confirm no order when flat (net 0) and a SELL when long.
	sub0 := &fakeSub{net: 0}
	_ = s.submit(sub0, sepa.Signal{Symbol: "AAPL", Side: sepa.SideFlat}, time.Now())
	if len(sub0.orders) != 0 {
		t.Fatalf("flat book should produce no close order, got %d", len(sub0.orders))
	}
	_ = s.submit(sub, sepa.Signal{Symbol: "AAPL", Side: sepa.SideFlat}, time.Now())
	if len(sub.orders) != 1 || sub.orders[0].side != domain.OrderSideSell || sub.orders[0].qty != 100 {
		t.Fatalf("FLAT should SELL 100, got %+v", sub.orders)
	}
}

func TestStateRoundTripJSON(t *testing.T) {
	s := newAdapter(t, "SEPA-AAPL", newGen(t, "AAPL"))
	s.InjectContext(engine.StrategyContext{Regime: "bull", MarketCapUSD: map[string]float64{"AAPL": 9e9}})
	snap := s.StateDictJSON()
	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	s2 := newAdapter(t, "SEPA-AAPL", newGen(t, "AAPL"))
	if err := s2.LoadStateJSON(b); err != nil {
		t.Fatalf("load: %v", err)
	}
	js := s2.StateSummaryJSON().(summaryJSON)
	if js.Regime != "bull" || js.MarketCapUSD != 9e9 {
		t.Fatalf("state not restored: %+v", js)
	}
}

// TestEvaluateSignalJSONReturnsDomainType pins the contract that the publish
// layer relies on AFTER the §E3 domain-bridge relocation: EvaluateSignalJSON
// returns the canonical domain.SEPASignal (NOT the pure sepa.SignalIntent
// and NOT a private adapter struct). publish.NormalizeIntent now switches only on
// domain types; returning anything else aborts every SEPA/multi intent in the
// signal/paper/live/EOD modes with "unsupported intent type". This is the
// in-package half of the regression guard; the wire-shape half is
// TestNormalizeIntentWireShape below + publish/intent_test.go
// TestNormalizeSEPAAdapterOutput.
func TestEvaluateSignalJSONReturnsDomainType(t *testing.T) {
	s := newAdapter(t, "SEPA-AAPL", newGen(t, "AAPL"))
	v := s.EvaluateSignalJSON(time.Date(2024, 1, 1, 21, 0, 0, 0, time.UTC))
	if _, ok := v.(domain.SEPASignal); !ok {
		t.Fatalf("EvaluateSignalJSON must return domain.SEPASignal for publish.NormalizeIntent; got %T", v)
	}
}

// TestNormalizeIntentWireShape proves the relocated sepaadapter.NormalizeIntent
// converts a pure sepa.SignalIntent (no json tags) to the spec-faithful
// snake_case domain wire shape — the coverage formerly in publish.
func TestNormalizeIntentWireShape(t *testing.T) {
	prox := 1.5
	in := sepa.SignalIntent{
		Symbol:              "AAPL",
		State:               sepa.StateBuy,
		Strength:            75,
		ProximityToTriggerP: &prox,
		UpdatedAt:           time.Date(2026, 6, 12, 0, 0, 0, 0, time.UTC),
		Generation:          7,
		StrategyID:          "sepa",
		Grade:               75,
		TrendTemplatePass:   true,
		PivotPrice:          "123.45",
		StopPrice:           "118.00",
	}
	d := NormalizeIntent(in)
	if d.Symbol != "AAPL" || d.State != domain.StateBuy || d.Strength != 75 || d.Generation != 7 {
		t.Fatalf("discriminators wrong: %+v", d)
	}
	if d.ProximityToTriggerPct == nil || *d.ProximityToTriggerPct != 1.5 {
		t.Fatalf("proximity not carried: %+v", d.ProximityToTriggerPct)
	}
	body, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	// Spec field names (snake_case), NOT the local Go field names.
	for k, want := range map[string]any{
		"strategy_id": "sepa", "symbol": "AAPL", "state": "buy",
		"grade": float64(75), "trend_template_pass": true,
	} {
		if m[k] != want {
			t.Fatalf("wire %q = %v, want %v", k, m[k], want)
		}
	}
	for _, k := range []string{"pivot_price", "proximity_to_trigger_pct"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("wire missing key %q", k)
		}
	}
}

// TestNormalizeIntentCarriesTradePlan proves the TMS-enhancement trade-plan
// fields pass through the adapter into the domain wire shape (snake_case JSONB).
func TestNormalizeIntentCarriesTradePlan(t *testing.T) {
	risk, pctOff, vol, ready := 6.5, -3.2, 2.1, 73.4
	in := sepa.SignalIntent{
		Symbol:       "AAPL",
		State:        sepa.StateForming,
		StrategyID:   "sepa",
		PivotPrice:   "120.00",
		StopPrice:    "111.00",
		RiskPct:      &risk,
		PctOff52wkH:  &pctOff,
		VolRatio:     &vol,
		BuyReadiness: &ready,
	}
	d := NormalizeIntent(in)
	if d.RiskPct == nil || *d.RiskPct != risk {
		t.Fatalf("risk_pct not carried: %+v", d.RiskPct)
	}
	if d.PctOff52wkHigh == nil || *d.PctOff52wkHigh != pctOff {
		t.Fatalf("pct_off_52wk_high not carried: %+v", d.PctOff52wkHigh)
	}
	if d.VolRatio == nil || *d.VolRatio != vol {
		t.Fatalf("vol_ratio not carried: %+v", d.VolRatio)
	}
	if d.BuyReadiness == nil || *d.BuyReadiness != ready {
		t.Fatalf("buy_readiness not carried: %+v", d.BuyReadiness)
	}
	body, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"risk_pct", "pct_off_52wk_high", "vol_ratio", "buy_readiness"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("wire missing trade-plan key %q", k)
		}
	}
}

var _ = mkBar // reserved for future end-to-end engine wiring tests
