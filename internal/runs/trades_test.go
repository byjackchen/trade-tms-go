package runs

import (
	"testing"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/domain"
)

func ts(day int) time.Time {
	return time.Date(2024, 1, day, 0, 0, 0, 0, time.UTC)
}

func fill(strat, sym string, side domain.OrderSide, qty int64, px string, day int) domain.Fill {
	return domain.Fill{
		TradeID:       sym + "-" + side.String() + "-" + px,
		ClientOrderID: "c",
		StrategyID:    strat,
		Symbol:        sym,
		Side:          side,
		Qty:           domain.Qty(qty),
		Price:         domain.MustPrice(px),
		TS:            ts(day),
	}
}

func TestExtractTradesLongRoundTrip(t *testing.T) {
	fills := []domain.Fill{
		fill("S", "AAPL", domain.OrderSideBuy, 100, "10.00", 1),
		fill("S", "AAPL", domain.OrderSideSell, 100, "12.00", 2),
	}
	trades, err := ExtractTrades(fills)
	if err != nil {
		t.Fatal(err)
	}
	if len(trades) != 1 {
		t.Fatalf("got %d trades, want 1", len(trades))
	}
	tr := trades[0]
	if tr.Side != "LONG" || tr.Qty != 100 {
		t.Fatalf("side/qty: %+v", tr)
	}
	if tr.EntryPx != domain.MustPrice("10.00") || tr.ExitPx == nil || *tr.ExitPx != domain.MustPrice("12.00") {
		t.Fatalf("prices: %+v", tr)
	}
	// PnL = (12-10)*100 = 200.
	if tr.RealizedPnL != domain.MustMoney("200.00") {
		t.Fatalf("pnl: %s", tr.RealizedPnL)
	}
	if tr.ExitTS == nil || !tr.ExitTS.Equal(ts(2)) {
		t.Fatalf("exit ts: %+v", tr.ExitTS)
	}
}

func TestExtractTradesShortRoundTrip(t *testing.T) {
	fills := []domain.Fill{
		fill("S", "KO", domain.OrderSideSell, 50, "20.00", 1),
		fill("S", "KO", domain.OrderSideBuy, 50, "18.00", 3),
	}
	trades, err := ExtractTrades(fills)
	if err != nil {
		t.Fatal(err)
	}
	if len(trades) != 1 {
		t.Fatalf("got %d trades", len(trades))
	}
	tr := trades[0]
	if tr.Side != "SHORT" {
		t.Fatalf("side: %s", tr.Side)
	}
	// Short PnL = (entry - exit) * qty = (20-18)*50 = 100.
	if tr.RealizedPnL != domain.MustMoney("100.00") {
		t.Fatalf("pnl: %s", tr.RealizedPnL)
	}
}

func TestExtractTradesScaleInThenOut(t *testing.T) {
	fills := []domain.Fill{
		fill("S", "MSFT", domain.OrderSideBuy, 100, "10.00", 1),
		fill("S", "MSFT", domain.OrderSideBuy, 100, "12.00", 2), // avg entry 11
		fill("S", "MSFT", domain.OrderSideSell, 200, "13.00", 3),
	}
	trades, err := ExtractTrades(fills)
	if err != nil {
		t.Fatal(err)
	}
	if len(trades) != 1 {
		t.Fatalf("got %d trades", len(trades))
	}
	tr := trades[0]
	if tr.Qty != 200 {
		t.Fatalf("peak qty: %d", tr.Qty)
	}
	if tr.EntryPx != domain.MustPrice("11.00") {
		t.Fatalf("avg entry: %s", tr.EntryPx)
	}
	// PnL = (13-11)*200 = 400.
	if tr.RealizedPnL != domain.MustMoney("400.00") {
		t.Fatalf("pnl: %s", tr.RealizedPnL)
	}
}

func TestExtractTradesReversal(t *testing.T) {
	// Buy 100, then sell 150: closes the long (100) and opens a short (50).
	fills := []domain.Fill{
		fill("S", "T", domain.OrderSideBuy, 100, "10.00", 1),
		fill("S", "T", domain.OrderSideSell, 150, "12.00", 2),
		fill("S", "T", domain.OrderSideBuy, 50, "11.00", 3), // close the short
	}
	trades, err := ExtractTrades(fills)
	if err != nil {
		t.Fatal(err)
	}
	if len(trades) != 2 {
		t.Fatalf("got %d trades, want 2 (long close + short close)", len(trades))
	}
	long, short := trades[0], trades[1]
	if long.Side != "LONG" || long.Qty != 100 {
		t.Fatalf("long: %+v", long)
	}
	// Long PnL = (12-10)*100 = 200.
	if long.RealizedPnL != domain.MustMoney("200.00") {
		t.Fatalf("long pnl: %s", long.RealizedPnL)
	}
	if short.Side != "SHORT" || short.Qty != 50 {
		t.Fatalf("short: %+v", short)
	}
	// Short opened at 12, closed at 11 => (12-11)*50 = 50.
	if short.RealizedPnL != domain.MustMoney("50.00") {
		t.Fatalf("short pnl: %s", short.RealizedPnL)
	}
}

func TestExtractTradesOpenAtEnd(t *testing.T) {
	fills := []domain.Fill{
		fill("S", "NVDA", domain.OrderSideBuy, 10, "100.00", 1),
	}
	trades, err := ExtractTrades(fills)
	if err != nil {
		t.Fatal(err)
	}
	if len(trades) != 1 {
		t.Fatalf("got %d trades", len(trades))
	}
	tr := trades[0]
	if tr.ExitTS != nil || tr.ExitPx != nil {
		t.Fatalf("open trade must have nil exit: %+v", tr)
	}
	if tr.RealizedPnL != 0 {
		t.Fatalf("open trade realized: %s", tr.RealizedPnL)
	}
}

func TestExtractTradesMultiStrategySymbol(t *testing.T) {
	// Two strategies trade the same symbol: separate positions (NETTING per
	// (strategy, instrument)).
	fills := []domain.Fill{
		fill("A", "X", domain.OrderSideBuy, 10, "5.00", 1),
		fill("B", "X", domain.OrderSideSell, 10, "5.00", 1),
		fill("A", "X", domain.OrderSideSell, 10, "6.00", 2),
		fill("B", "X", domain.OrderSideBuy, 10, "4.00", 2),
	}
	trades, err := ExtractTrades(fills)
	if err != nil {
		t.Fatal(err)
	}
	if len(trades) != 2 {
		t.Fatalf("got %d trades, want 2", len(trades))
	}
	// Sorted by strategy_id: A (long, +10), then B (short, +10).
	if trades[0].StrategyID != "A" || trades[0].Side != "LONG" {
		t.Fatalf("trade0: %+v", trades[0])
	}
	if trades[1].StrategyID != "B" || trades[1].Side != "SHORT" {
		t.Fatalf("trade1: %+v", trades[1])
	}
}

func TestExtractTradesEmpty(t *testing.T) {
	trades, err := ExtractTrades(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(trades) != 0 {
		t.Fatalf("got %d trades, want 0", len(trades))
	}
}
