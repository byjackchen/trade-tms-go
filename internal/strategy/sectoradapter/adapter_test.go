package sectoradapter

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/engine"
	"github.com/byjackchen/trade-tms-go/internal/strategy/sectorrotation"
)

// recSub records SubmitMarket calls and answers net-position queries, standing
// in for the engine's submitter+position reader during on_bar.
type recSub struct {
	calls []call
	net   map[string]domain.Qty
}

type call struct {
	sym  string
	side domain.OrderSide
	qty  domain.Qty
}

func (r *recSub) SubmitMarket(strategyID, symbol string, side domain.OrderSide, qty domain.Qty, reason string, ts time.Time) (string, error) {
	r.calls = append(r.calls, call{symbol, side, qty})
	return "cid", nil
}

func (r *recSub) SubmitMarketSignal(id, symbol string, _ domain.SignalSide, side domain.OrderSide, qty domain.Qty, reason string, ts time.Time) (string, bool, error) {
	coid, err := r.SubmitMarket(id, symbol, side, qty, reason, ts)
	return coid, err == nil, err
}

func (r *recSub) NetPosition(strategyID, symbol string) domain.Qty { return r.net[symbol] }

// Ensure recSub also satisfies the PositionReader seam the adapter probes.
var _ engine.PositionReader = (*recSub)(nil)

func mkAdapter(t *testing.T) *Strategy {
	t.Helper()
	sg, err := sectorrotation.New(sectorrotation.Config{
		EquityProvider:   func() float64 { return 100000 },
		Universe:         []string{"AAA", "BBB", "CCC", "DDD"},
		MomentumLookback: 20,
		TopK:             2,
		Timezone:         "America/New_York",
	})
	if err != nil {
		t.Fatalf("sectorrotation.New: %v", err)
	}
	a, err := New("SectorRotationRunner-000", sg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

func barOf(sym string, ts time.Time, close float64) domain.Bar {
	p, _ := domain.PriceFromFloat64(close)
	return domain.Bar{Symbol: sym, TS: ts, Open: p, High: p, Low: p, Close: p, Volume: 1}
}

// TestAdapterTranslatesRebalanceToOrders drives a cold-start month rollover and
// asserts the adapter emits BUY orders of the SG's target_qty for the new
// top-K (LONG -> BUY full target).
func TestAdapterTranslatesRebalanceToOrders(t *testing.T) {
	a := mkAdapter(t)
	sub := &recSub{net: map[string]domain.Qty{}}
	start := time.Date(2024, 1, 4, 0, 0, 0, 0, time.UTC)
	closes := map[string][]float64{
		"AAA": {}, "BBB": {}, "CCC": {}, "DDD": {},
	}
	for i := 0; i < 30; i++ {
		closes["AAA"] = append(closes["AAA"], 100.0+float64(i)*1.0)
		closes["BBB"] = append(closes["BBB"], 50.0+float64(i)*0.5)
		closes["CCC"] = append(closes["CCC"], 100.0)
		closes["DDD"] = append(closes["DDD"], 100.0-float64(i)*0.5)
	}
	for day := 0; day < 30; day++ {
		ts := start.AddDate(0, 0, day)
		for _, sym := range []string{"AAA", "BBB", "CCC", "DDD"} {
			if err := a.OnBar(sub, barOf(sym, ts, closes[sym][day])); err != nil {
				t.Fatalf("OnBar: %v", err)
			}
		}
	}
	if len(sub.calls) != 2 {
		t.Fatalf("expected 2 BUY orders, got %d: %+v", len(sub.calls), sub.calls)
	}
	for _, c := range sub.calls {
		if c.side != domain.OrderSideBuy {
			t.Errorf("order %s side = %s, want BUY", c.sym, c.side)
		}
		if c.qty <= 0 {
			t.Errorf("order %s qty = %d", c.sym, c.qty)
		}
	}
}

// TestAdapterFlatClosesLiveNetPosition verifies the FLAT branch reads the live
// net position from the PositionReader seam and submits a closing SELL.
func TestAdapterFlatClosesLiveNetPosition(t *testing.T) {
	a := mkAdapter(t)
	sg := a.Generator()
	// Seed: AAA held 100 sh, last close set; trigger a FLAT by a hand-built
	// rebalance signal path is internal — instead drive a rotation. Simpler:
	// mark AAA held then feed a month-2 rollover where AAA drops out.
	// Drive phase 1 (AAA/BBB win), then phase 2 (CCC/DDD win) -> AAA FLAT.
	sub := &recSub{net: map[string]domain.Qty{}}
	start := time.Date(2024, 1, 4, 0, 0, 0, 0, time.UTC)
	p1 := func(i int) map[string]float64 {
		return map[string]float64{
			"AAA": 100.0 + float64(i)*1.0, "BBB": 50.0 + float64(i)*0.5,
			"CCC": 100.0, "DDD": 100.0 - float64(i)*0.5,
		}
	}
	for day := 0; day < 30; day++ {
		ts := start.AddDate(0, 0, day)
		for _, sym := range []string{"AAA", "BBB", "CCC", "DDD"} {
			_ = a.OnBar(sub, barOf(sym, ts, p1(day)[sym]))
		}
	}
	// Reflect the orders into the recorded net so the FLAT branch sees real qty.
	for _, c := range sub.calls {
		if c.side == domain.OrderSideBuy {
			sub.net[c.sym] += c.qty
		}
	}
	sub.calls = nil

	start2 := time.Date(2024, 2, 3, 0, 0, 0, 0, time.UTC)
	p2 := func(i int) map[string]float64 {
		return map[string]float64{
			"AAA": 129.0 - float64(i)*0.5, "BBB": 64.5 - float64(i)*0.3,
			"CCC": 100.0 + float64(i)*1.5, "DDD": 85.5 + float64(i)*1.0,
		}
	}
	for day := 0; day < 30; day++ {
		ts := start2.AddDate(0, 0, day)
		for _, sym := range []string{"AAA", "BBB", "CCC", "DDD"} {
			if err := a.OnBar(sub, barOf(sym, ts, p2(day)[sym])); err != nil {
				t.Fatalf("OnBar phase2: %v", err)
			}
		}
	}
	var sells, buys int
	for _, c := range sub.calls {
		switch c.side {
		case domain.OrderSideSell:
			sells++ // AAA/BBB closes
		case domain.OrderSideBuy:
			buys++ // CCC/DDD entries
		}
	}
	if sells != 2 || buys != 2 {
		t.Errorf("rotation orders: sells=%d buys=%d, want 2/2 (calls=%+v)", sells, buys, sub.calls)
	}
	_ = sg
}

// TestAdapterCapabilitySeams exercises the telemetry/persistence seams.
func TestAdapterCapabilitySeams(t *testing.T) {
	a := mkAdapter(t)
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	intents := a.EvaluateIntentJSON(now)
	if _, err := json.Marshal(intents); err != nil {
		t.Errorf("intents not JSON-serializable: %v", err)
	}
	summary := a.StateSummaryJSON()
	if _, err := json.Marshal(summary); err != nil {
		t.Errorf("summary not JSON-serializable: %v", err)
	}
	sd := a.StateDictJSON()
	b, err := json.Marshal(sd)
	if err != nil {
		t.Fatalf("state_dict marshal: %v", err)
	}
	if err := a.LoadStateJSON(b); err != nil {
		t.Errorf("LoadStateJSON round-trip: %v", err)
	}
}
