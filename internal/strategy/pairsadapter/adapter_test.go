package pairsadapter

// adapter_test.go: the engine seam — Signal -> market order translation
// (strategy-pairs.md §10) and capability-interface wiring.

import (
	"testing"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/engine"
	"github.com/byjackchen/trade-tms-go/internal/strategy/pairs"
)

// fakeSub records submitted orders and answers net-position reads (it doubles
// as engine.OrderSubmitter + engine.PositionReader, like the engine's own
// orderSubmitter/accountPositionReader wiring).
type fakeSub struct {
	orders []order
	net    map[string]domain.Qty
}

type order struct {
	symbol string
	side   domain.OrderSide
	qty    domain.Qty
	reason string
}

func (f *fakeSub) SubmitMarket(_, symbol string, side domain.OrderSide, qty domain.Qty, reason string, _ time.Time) (string, error) {
	f.orders = append(f.orders, order{symbol, side, qty, reason})
	return "coid", nil
}
func (f *fakeSub) SubmitMarketSignal(id, symbol string, _ domain.SignalSide, side domain.OrderSide, qty domain.Qty, reason string, ts time.Time) (string, bool, error) {
	coid, err := f.SubmitMarket(id, symbol, side, qty, reason, ts)
	return coid, err == nil, err
}
func (f *fakeSub) NetPosition(_, symbol string) domain.Qty { return f.net[symbol] }

func newGen(t *testing.T, lookback int, entryZ float64) *pairs.Generator {
	t.Helper()
	g, err := pairs.New(pairs.Config{
		EquityProvider:    pairs.ConstantEquity(100000),
		Pairs:             []pairs.Pair{{LongLeg: "KO", ShortLeg: "PEP"}},
		Lookback:          lookback,
		EntryZ:            entryZ,
		ExitZ:             0.5,
		CapitalPerPairPct: 0.30,
		Timezone:          "America/New_York",
	})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func bar(sym, date, close string) domain.Bar {
	ts, _ := time.Parse("2006-01-02", date)
	p := domain.MustPrice(close)
	return domain.Bar{Symbol: sym, TS: ts.UTC(), Open: p, High: p, Low: p, Close: p, Volume: 1}
}

func TestAdapterCapabilities(t *testing.T) {
	a, err := New("Pairs-002", newGen(t, 60, 2.0))
	if err != nil {
		t.Fatal(err)
	}
	var _ engine.Strategy = a
	if _, ok := any(a).(engine.IntentEvaluator); !ok {
		t.Error("not IntentEvaluator")
	}
	if _, ok := any(a).(engine.StateSummarizer); !ok {
		t.Error("not StateSummarizer")
	}
	if _, ok := any(a).(engine.StatePersister); !ok {
		t.Error("not StatePersister")
	}
	// Pairs deliberately does NOT consume context.
	if _, ok := any(a).(engine.ContextConsumer); ok {
		t.Error("Pairs must not implement ContextConsumer")
	}
	if a.ID() != "Pairs-002" {
		t.Fatalf("id %q", a.ID())
	}
}

func TestNewValidation(t *testing.T) {
	if _, err := New("", newGen(t, 60, 2.0)); err == nil {
		t.Error("empty id should error")
	}
	if _, err := New("Pairs-002", nil); err == nil {
		t.Error("nil generator should error")
	}
}

func TestEntryTranslation(t *testing.T) {
	// Spread z spikes to +1.957 on the last bar (> entry_z 1.5) => SHORT_SPREAD:
	// SHORT long_leg KO (SELL), LONG short_leg PEP (BUY); long_leg first.
	a, _ := New("Pairs-002", newGen(t, 5, 1.5))
	sub := &fakeSub{net: map[string]domain.Qty{}}
	ko := []string{"100", "100.5", "99.5", "100.2", "108"}
	pep := []string{"200", "200.1", "199.9", "200.2", "200"}
	for i := range ko {
		d := time.Date(2020, 1, 2, 0, 0, 0, 0, time.UTC).AddDate(0, 0, i)
		_ = a.OnBar(sub, bar("KO", d.Format("2006-01-02"), ko[i]))
		_ = a.OnBar(sub, bar("PEP", d.Format("2006-01-02"), pep[i]))
	}
	if len(sub.orders) != 2 {
		t.Fatalf("expected 2 entry orders, got %d: %+v", len(sub.orders), sub.orders)
	}
	if sub.orders[0].symbol != "KO" || sub.orders[0].side != domain.OrderSideSell {
		t.Fatalf("order0 %+v", sub.orders[0])
	}
	if sub.orders[1].symbol != "PEP" || sub.orders[1].side != domain.OrderSideBuy {
		t.Fatalf("order1 %+v", sub.orders[1])
	}
}

func TestFlatSizesFromNetPosition(t *testing.T) {
	// Drive an entry, then a close, and assert the FLATs size from the broker
	// net (provided via the submitter's PositionReader), not the SG leg slot.
	a, _ := New("Pairs-002", newGen(t, 5, 1.5))
	// After the SHORT_SPREAD entry above, KO is short and PEP is long. Simulate
	// the broker net the engine would report.
	sub := &fakeSub{net: map[string]domain.Qty{"KO": -350, "PEP": 124}}
	// Day 5 spikes z to +1.957 (entry); day 6 reverts z to -0.08 (|z|<exit_z)
	// => mean-reversion close.
	ko := []string{"100", "100.5", "99.5", "100.2", "108", "102"}
	pep := []string{"200", "200.1", "199.9", "200.2", "200", "200"}
	for i := range ko {
		d := time.Date(2020, 1, 2, 0, 0, 0, 0, time.UTC).AddDate(0, 0, i)
		_ = a.OnBar(sub, bar("KO", d.Format("2006-01-02"), ko[i]))
		_ = a.OnBar(sub, bar("PEP", d.Format("2006-01-02"), pep[i]))
	}
	// On the last day the spread reverts toward 0 (|z| < exit_z 0.5) => close.
	// The two close orders size from the broker net: KO net -350 -> BUY 350;
	// PEP net +124 -> SELL 124.
	var closes []order
	for _, o := range sub.orders {
		if o.reason != "" && containsFLAT(o.reason) {
			closes = append(closes, o)
		}
	}
	if len(closes) != 2 {
		t.Fatalf("expected 2 close orders, got %d (all: %+v)", len(closes), sub.orders)
	}
	for _, o := range closes {
		switch o.symbol {
		case "KO":
			if o.side != domain.OrderSideBuy || o.qty != 350 {
				t.Fatalf("KO close %+v", o)
			}
		case "PEP":
			if o.side != domain.OrderSideSell || o.qty != 124 {
				t.Fatalf("PEP close %+v", o)
			}
		}
	}
}

func containsFLAT(s string) bool {
	for i := 0; i+4 <= len(s); i++ {
		if s[i:i+4] == "FLAT" {
			return true
		}
	}
	return false
}
