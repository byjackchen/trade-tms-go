package engine_test

// orb_seam_test.go pins the ORB (intraday_breakout) engine adapter to the
// Strategy seam and the locked-decision-3 capability interfaces. It lives in the
// engine package (not the orb package) so the PURE strategy package never
// imports the engine — preserving the Eng-D2 two-layer contract. A build break
// here means the ORB adapter drifted from the seam. It also exercises a full
// entry -> EOD-flat round trip through the seam to confirm the LONG/FLAT order
// translation and the net-position FLAT sizing.

import (
	"testing"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/engine"
	"github.com/byjackchen/trade-tms-go/internal/strategy/orb"
	"github.com/byjackchen/trade-tms-go/internal/strategy/orbadapter"
)

// Compile-time guarantees: *orbadapter.Strategy implements the base Strategy
// seam plus every capability interface the engine probes for. ORB consumes no
// per-bar context, so ContextConsumer is intentionally absent.
var (
	_ engine.Strategy        = (*orbadapter.Strategy)(nil)
	_ engine.IntentEvaluator = (*orbadapter.Strategy)(nil)
	_ engine.StateSummarizer = (*orbadapter.Strategy)(nil)
	_ engine.StatePersister  = (*orbadapter.Strategy)(nil)
)

// orbSubmitter records orders and answers net-position queries (implements both
// OrderSubmitter and PositionReader, like the real engine submitter).
type orbSubmitter struct {
	orders []orbOrder
	net    domain.Qty
}

type orbOrder struct {
	symbol string
	side   domain.OrderSide
	qty    domain.Qty
	reason string
}

func (s *orbSubmitter) SubmitMarket(_, symbol string, side domain.OrderSide, qty domain.Qty, reason string, _ time.Time) (string, error) {
	s.orders = append(s.orders, orbOrder{symbol, side, qty, reason})
	return "coid", nil
}

func (s *orbSubmitter) SubmitMarketSignal(id, symbol string, _ domain.SignalSide, side domain.OrderSide, qty domain.Qty, reason string, ts time.Time) (string, bool, error) {
	coid, err := s.SubmitMarket(id, symbol, side, qty, reason, ts)
	return coid, err == nil, err
}

func (s *orbSubmitter) NetPosition(_, _ string) domain.Qty { return s.net }

func orbSeamBar(t *testing.T, h, mi int, o, hi, lo, c string, v int64) domain.Bar {
	t.Helper()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Date(2024, time.January, 8, h, mi, 0, 0, loc).UTC()
	return domain.Bar{
		Symbol: "AAPL", TS: ts,
		Open:   domain.MustPrice(o),
		High:   domain.MustPrice(hi),
		Low:    domain.MustPrice(lo),
		Close:  domain.MustPrice(c),
		Volume: v,
	}
}

func TestORBAdapterImplementsSeamAndTrades(t *testing.T) {
	cfg := orb.DefaultConfig()
	cfg.Symbol = "AAPL"
	cfg.EquityProvider = func() float64 { return 100000 }
	gen, err := orb.New(cfg)
	if err != nil {
		t.Fatalf("orb.New: %v", err)
	}
	a, err := orbadapter.New("IntradayBreakoutRunner-000", gen)
	if err != nil {
		t.Fatalf("orbadapter.New: %v", err)
	}
	if a.ID() != "IntradayBreakoutRunner-000" {
		t.Fatalf("ID = %q", a.ID())
	}

	sub := &orbSubmitter{}

	// Opening range 09:30..09:55.
	for i := 0; i < 6; i++ {
		if err := a.OnBar(sub, orbSeamBar(t, 9, 30+i*5, "100.00", "101.00", "99.00", "100.00", 1_000_000)); err != nil {
			t.Fatalf("range OnBar: %v", err)
		}
	}
	if len(sub.orders) != 0 {
		t.Fatalf("range emitted orders: %+v", sub.orders)
	}

	// Breakout -> LONG BUY 980.
	if err := a.OnBar(sub, orbSeamBar(t, 10, 5, "101.00", "102.50", "101.00", "102.00", 2_000_000)); err != nil {
		t.Fatalf("breakout OnBar: %v", err)
	}
	if len(sub.orders) != 1 || sub.orders[0].side != domain.OrderSideBuy || sub.orders[0].qty != 980 {
		t.Fatalf("expected BUY 980, got %+v", sub.orders)
	}
	if !hasSub(sub.orders[0].reason, "[IntradayBreakout] LONG 980 AAPL") {
		t.Fatalf("LONG reason = %q", sub.orders[0].reason)
	}

	// Position now live; EOD FLAT closes the real net via SELL.
	sub.net = 980
	if err := a.OnBar(sub, orbSeamBar(t, 15, 55, "102.50", "102.80", "102.00", "102.50", 500_000)); err != nil {
		t.Fatalf("eod OnBar: %v", err)
	}
	if len(sub.orders) != 2 || sub.orders[1].side != domain.OrderSideSell || sub.orders[1].qty != 980 {
		t.Fatalf("expected SELL 980 FLAT, got %+v", sub.orders)
	}
	if !hasSub(sub.orders[1].reason, "FLAT (close 980) AAPL") || !hasSub(sub.orders[1].reason, "EOD exit at 15:55") {
		t.Fatalf("FLAT reason = %q", sub.orders[1].reason)
	}

	// Capability surfaces.
	sub.net = 0
	// §E3: the adapter is the domain bridge — EvaluateIntentJSON returns the
	// canonical domain.IntradayBreakoutIntent (not the pure orb.SignalIntent).
	if it, ok := a.EvaluateIntentJSON(orbSeamBar(t, 15, 55, "1", "1", "1", "1", 0).TS).(domain.IntradayBreakoutIntent); !ok || it.StrategyID != orb.StrategyID {
		t.Fatalf("EvaluateIntentJSON returned %T", a.EvaluateIntentJSON(time.Now()))
	}
	if sm, ok := a.StateSummaryJSON().(orb.StateSummary); !ok || sm.Symbol != "AAPL" {
		t.Fatalf("StateSummaryJSON returned %T", a.StateSummaryJSON())
	}
	if sd, ok := a.StateDictJSON().(orb.StateDict); !ok || sd.Config.Symbol != "AAPL" {
		t.Fatalf("StateDictJSON returned %T", a.StateDictJSON())
	}
}

func hasSub(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
