package runs

import (
	"context"
	"testing"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/engine"
)

// memFeed is an in-memory BarFeed for engine-level assembly tests.
type memFeed struct{ instruments []engine.InstrumentBars }

func (f memFeed) Load(_ context.Context, _ []string, _, _ calendar.Date) ([]engine.InstrumentBars, error) {
	return f.instruments, nil
}

func bar(sym string, day int, close string) domain.Bar {
	px := domain.MustPrice(close)
	return domain.Bar{
		Symbol: sym,
		TS:     time.Date(2024, 1, day, 0, 0, 0, 0, time.UTC),
		Open:   px, High: px, Low: px, Close: px, Volume: 1000,
	}
}

// TestAssembleEndToEnd runs the engine over a scripted long round trip and
// asserts the assembled metrics, trades and equity reconcile with the result.
func TestAssembleEndToEnd(t *testing.T) {
	feed := memFeed{instruments: []engine.InstrumentBars{
		{Symbol: "AAPL", Bars: []domain.Bar{
			bar("AAPL", 2, "10.00"),
			bar("AAPL", 3, "11.00"),
			bar("AAPL", 4, "12.00"),
		}},
	}}
	cfg := engine.Config{
		Tickers:         []string{"AAPL"},
		Start:           calendar.NewDate(2024, 1, 2),
		End:             calendar.NewDate(2024, 1, 4),
		StartingBalance: domain.MustMoney("100000.00"),
		Profile:         engine.ProfileCloseFill,
		Strategies: []engine.StrategySpec{{
			ID: "Scripted-000",
			Intents: []engine.Intent{
				{Date: time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC), Ticker: "AAPL", Side: domain.SideLong, Qty: 100},
				{Date: time.Date(2024, 1, 4, 0, 0, 0, 0, time.UTC), Ticker: "AAPL", Side: domain.SideFlat},
			},
		}},
	}

	eng, err := engine.New(context.Background(), cfg, feed)
	if err != nil {
		t.Fatal(err)
	}
	res, err := eng.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	a, err := Assemble(res, AssembleParams{
		RunTS:     "2024-06-13_12-00-00",
		Kind:      "smoke",
		StartDate: cfg.Start,
		EndDate:   cfg.End,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Bought 100 @ 10, sold 100 @ 12 (close-fill fills at the bar close):
	// realized = 200; final balance = 100200.
	if res.FinalBalance != domain.MustMoney("100200.00") {
		t.Fatalf("final balance: %s", res.FinalBalance)
	}
	if len(a.Persist.Trades) != 1 {
		t.Fatalf("got %d trades, want 1", len(a.Persist.Trades))
	}
	tr := a.Persist.Trades[0]
	if tr.Side != "LONG" || tr.RealizedPnL != domain.MustMoney("200.00") {
		t.Fatalf("trade: %+v", tr)
	}

	pm := a.Persist.PortfolioMetrics
	if pm.FinalBalanceUSD != 100200.0 || pm.TotalPnLUSD != 200.0 {
		t.Fatalf("portfolio metrics balances: %+v", pm)
	}
	if pm.NumOrders != 2 || pm.NumFilledOrders != 2 {
		t.Fatalf("counters: %+v", pm)
	}
	// One position opened (AAPL under Scripted-000).
	if pm.NumPositions != 1 {
		t.Fatalf("num positions: %d", pm.NumPositions)
	}
	// Per-strategy metrics exist.
	if _, ok := a.Persist.StrategyMetrics["Scripted-000"]; !ok {
		t.Fatal("missing per-strategy metrics")
	}
	// Portfolio equity curve is non-empty and ends at the final balance.
	if n := len(a.Persist.PortfolioEquity); n == 0 {
		t.Fatal("empty portfolio equity")
	}
	last := a.Persist.PortfolioEquity[len(a.Persist.PortfolioEquity)-1]
	if last.BalanceUSD != res.FinalBalance {
		t.Fatalf("equity tail %s != final %s", last.BalanceUSD, res.FinalBalance)
	}
	// Artifact carries the same identity.
	if a.Artifact.TS != "2024-06-13_12-00-00" || a.Artifact.Kind != "smoke" {
		t.Fatalf("artifact identity: %+v", a.Artifact)
	}
	if a.Artifact.StartDate != "2024-01-02" || a.Artifact.EndDate != "2024-01-04" {
		t.Fatalf("artifact dates: %s..%s", a.Artifact.StartDate, a.Artifact.EndDate)
	}
}

func TestAssembleNoTrades(t *testing.T) {
	feed := memFeed{instruments: []engine.InstrumentBars{
		{Symbol: "AAPL", Bars: []domain.Bar{bar("AAPL", 2, "10.00"), bar("AAPL", 3, "10.00")}},
	}}
	cfg := engine.Config{
		Tickers:         []string{"AAPL"},
		Start:           calendar.NewDate(2024, 1, 2),
		End:             calendar.NewDate(2024, 1, 3),
		StartingBalance: domain.MustMoney("100000.00"),
		Strategies:      []engine.StrategySpec{{ID: "Scripted-000"}}, // no intents
	}
	eng, err := engine.New(context.Background(), cfg, feed)
	if err != nil {
		t.Fatal(err)
	}
	res, err := eng.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	a, err := Assemble(res, AssembleParams{RunTS: "2024-06-13_12-00-00", StartDate: cfg.Start, EndDate: cfg.End})
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Persist.Trades) != 0 {
		t.Fatalf("got %d trades, want 0", len(a.Persist.Trades))
	}
	if a.Persist.PortfolioMetrics.NumOrders != 0 {
		t.Fatalf("orders: %d", a.Persist.PortfolioMetrics.NumOrders)
	}
}
