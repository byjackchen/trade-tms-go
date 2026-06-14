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
	g, err := sepa.NewGenerator(sepa.Config{
		Symbol: symbol, EquityProvider: func() float64 { return 100000 },
		RiskPct: 1.0, MarketCapMinUSD: 5e8, HardStopPct: 7.5, PivotBufferPct: 1.5,
		BreakoutVolumeMultiple: 1.5, VCPLookback: 4, HistoryMaxBars: 1000, Timezone: "America/New_York",
	})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func mkBar(sym string, ts time.Time, c float64, v int64) domain.Bar {
	p := func(f float64) domain.Price {
		x, _ := domain.PriceFromFloat64(f)
		return x
	}
	return domain.Bar{Symbol: sym, TS: ts, Open: p(c), High: p(c + 0.5), Low: p(c - 0.5), Close: p(c), Volume: v}
}

func TestAdapterImplementsCapabilities(t *testing.T) {
	var s engine.Strategy = New("SEPA-AAPL", newGen(t, "AAPL"))
	if _, ok := s.(engine.ContextConsumer); !ok {
		t.Error("not a ContextConsumer")
	}
	if _, ok := s.(engine.IntentEvaluator); !ok {
		t.Error("not an IntentEvaluator")
	}
	if _, ok := s.(engine.StateSummarizer); !ok {
		t.Error("not a StateSummarizer")
	}
	if _, ok := s.(engine.StatePersister); !ok {
		t.Error("not a StatePersister")
	}
}

func TestInjectContextRoutesBySymbol(t *testing.T) {
	s := New("SEPA-AAPL", newGen(t, "AAPL"))
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
	s := New("SEPA-AAPL", g)
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
	s := New("SEPA-AAPL", newGen(t, "AAPL"))
	s.InjectContext(engine.StrategyContext{Regime: "bull", MarketCapUSD: map[string]float64{"AAPL": 9e9}})
	snap := s.StateDictJSON()
	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	s2 := New("SEPA-AAPL", newGen(t, "AAPL"))
	if err := s2.LoadStateJSON(b); err != nil {
		t.Fatalf("load: %v", err)
	}
	js := s2.StateSummaryJSON().(summaryJSON)
	if js.Regime != "bull" || js.MarketCapUSD != 9e9 {
		t.Fatalf("state not restored: %+v", js)
	}
}

// TestEvaluateIntentJSONReturnsRawSepaType pins the contract that the publish
// layer relies on: EvaluateIntentJSON returns the RAW sepa.SignalIntent value
// (NOT a private adapter struct). publish.NormalizeIntent only has a case for
// sepa.SignalIntent; returning anything else aborts every SEPA/multi intent in
// the signal/paper/live/EOD modes with "unsupported intent type". This is the
// in-package half of the regression guard; the publish-side half asserts the
// resulting wire shape (publish/intent_test.go TestNormalizeSEPAAdapterOutput).
func TestEvaluateIntentJSONReturnsRawSepaType(t *testing.T) {
	s := New("SEPA-AAPL", newGen(t, "AAPL"))
	v := s.EvaluateIntentJSON(time.Date(2024, 1, 1, 21, 0, 0, 0, time.UTC))
	if _, ok := v.(sepa.SignalIntent); !ok {
		t.Fatalf("EvaluateIntentJSON must return sepa.SignalIntent for publish.NormalizeIntent; got %T", v)
	}
}

var _ = mkBar // reserved for future end-to-end engine wiring tests
