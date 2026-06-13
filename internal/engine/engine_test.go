package engine

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// barRow is a terse OHLCV row for building test feeds.
type barRow struct {
	y, m, d    int
	o, h, l, c string
	vol        int64
}

func ts(y, m, d int) time.Time { return time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC) }

func mkBars(symbol string, rows []barRow) InstrumentBars {
	bars := make([]domain.Bar, 0, len(rows))
	for _, r := range rows {
		bars = append(bars, domain.Bar{
			Symbol: symbol,
			TS:     ts(r.y, r.m, r.d),
			Open:   domain.MustPrice(r.o),
			High:   domain.MustPrice(r.h),
			Low:    domain.MustPrice(r.l),
			Close:  domain.MustPrice(r.c),
			Volume: r.vol,
		})
	}
	return InstrumentBars{Symbol: symbol, Bars: bars}
}

// aaplRows mirror the OHLC used in the Nautilus parity probe (tmp/parity_*.py).
var aaplRows = []barRow{
	{2025, 1, 2, "100.00", "110.00", "95.00", "105.00", 1000},
	{2025, 1, 3, "106.00", "112.00", "104.00", "108.00", 1000},
	{2025, 1, 6, "108.00", "109.00", "100.00", "101.00", 1000},
	{2025, 1, 7, "101.00", "103.00", "90.00", "92.00", 1000},
	{2025, 1, 8, "92.00", "99.00", "91.00", "98.00", 1000},
}

func newAAPLConfig(intents []Intent) Config {
	return Config{
		Tickers:         []string{"AAPL"},
		Start:           calendar.NewDate(2025, 1, 1),
		End:             calendar.NewDate(2025, 1, 31),
		StartingBalance: domain.MustMoney("100000"),
		Profile:         ProfileNautilusCompat,
		Strategies: []StrategySpec{
			{ID: "Scripted-000", Intents: intents},
		},
	}
}

func runEngine(t *testing.T, cfg Config, instruments ...InstrumentBars) *Result {
	t.Helper()
	feed := SliceFeed{Instruments: instruments}
	eng, err := New(context.Background(), cfg, feed)
	require.NoError(t, err)
	res, err := eng.Run(context.Background())
	require.NoError(t, err)
	return res
}

// TestParityLongPartialFlipFlat replicates the empirical Nautilus run:
// BUY 100 @105 (bar0 close), SELL 50 @108, SELL 100 @101 (flip to -50),
// BUY 50 @92 (flat). Final balance must be 100400.
func TestParityLongPartialFlipFlat(t *testing.T) {
	intents := []Intent{
		{Date: ts(2025, 1, 2), Ticker: "AAPL", Side: domain.SideLong, Qty: 100},
		{Date: ts(2025, 1, 3), Ticker: "AAPL", Side: domain.SideShort, Qty: 50},
		{Date: ts(2025, 1, 6), Ticker: "AAPL", Side: domain.SideShort, Qty: 100},
		{Date: ts(2025, 1, 7), Ticker: "AAPL", Side: domain.SideLong, Qty: 50},
	}
	res := runEngine(t, newAAPLConfig(intents), mkBars("AAPL", aaplRows))

	assert.Equal(t, domain.MustMoney("100400"), res.FinalBalance, "final balance must match Nautilus")
	assert.Equal(t, domain.MustMoney("400"), res.TotalPnL)
	assert.Len(t, res.Fills, 4)
	// Fill prices must be the bar closes (nautilus-compat).
	wantPx := []string{"105", "108", "101", "92"}
	for i, f := range res.Fills {
		assert.Equal(t, domain.MustPrice(wantPx[i]), f.Price, "fill %d price", i)
		assert.Equal(t, domain.Money(0), f.Commission, "fill %d zero commission", i)
	}
	// Position is flat at the end.
	require.Len(t, res.Positions, 1)
	assert.True(t, res.Positions[0].IsFlat())
}

// TestParityShortFirst replicates the SHORT-first margin case:
// SELL 100 @105 (bar0), BUY 100 @101 (bar2) -> realized 400.
func TestParityShortFirst(t *testing.T) {
	intents := []Intent{
		{Date: ts(2025, 1, 2), Ticker: "AAPL", Side: domain.SideShort, Qty: 100},
		{Date: ts(2025, 1, 6), Ticker: "AAPL", Side: domain.SideLong, Qty: 100},
	}
	res := runEngine(t, newAAPLConfig(intents), mkBars("AAPL", aaplRows))
	assert.Equal(t, domain.MustMoney("100400"), res.FinalBalance)
}

// TestParityAddToLong replicates averaging: BUY 100 @105, BUY 100 @108
// (avg 106.5), SELL 200 @92 -> realized 200*(92-106.5) = -2900.
func TestParityAddToLong(t *testing.T) {
	intents := []Intent{
		{Date: ts(2025, 1, 2), Ticker: "AAPL", Side: domain.SideLong, Qty: 100},
		{Date: ts(2025, 1, 3), Ticker: "AAPL", Side: domain.SideLong, Qty: 100},
		{Date: ts(2025, 1, 7), Ticker: "AAPL", Side: domain.SideShort, Qty: 200},
	}
	res := runEngine(t, newAAPLConfig(intents), mkBars("AAPL", aaplRows))
	assert.Equal(t, domain.MustMoney("97100"), res.FinalBalance)
	assert.Equal(t, domain.MustMoney("-2900"), res.TotalPnL)
}

// TestFlatClosesNetPosition exercises the FLAT intent (close via net position).
func TestFlatClosesNetPosition(t *testing.T) {
	intents := []Intent{
		{Date: ts(2025, 1, 2), Ticker: "AAPL", Side: domain.SideLong, Qty: 100},
		{Date: ts(2025, 1, 3), Ticker: "AAPL", Side: domain.SideFlat}, // close 100 @108
	}
	res := runEngine(t, newAAPLConfig(intents), mkBars("AAPL", aaplRows))
	// realized = 100*(108-105) = 300.
	assert.Equal(t, domain.MustMoney("100300"), res.FinalBalance)
	require.Len(t, res.Fills, 2)
	assert.Equal(t, domain.OrderSideSell, res.Fills[1].Side)
	assert.Equal(t, domain.Qty(100), res.Fills[1].Qty)
}

// TestAccountStatesCadence checks the account-state curve: one initial event
// plus one per fill (settlement), with balances tracking cumulative realized.
// The position is left NET +50 long after the two intents, so end-of-run
// liquidation (on_stop close_all_positions parity) flattens it on the last bar
// (2025-01-08 close 98), adding a 4th settling state: 50*(98-105)=-350 -> 99800.
func TestAccountStatesCadence(t *testing.T) {
	intents := []Intent{
		{Date: ts(2025, 1, 2), Ticker: "AAPL", Side: domain.SideLong, Qty: 100},
		{Date: ts(2025, 1, 3), Ticker: "AAPL", Side: domain.SideShort, Qty: 50},
	}
	res := runEngine(t, newAAPLConfig(intents), mkBars("AAPL", aaplRows))
	// initial(100000) + fill0(open: realized 0 -> 100000) + fill1(close 50:
	// 50*(108-105)=150 -> 100150) + liquidation(close remaining 50 @98:
	// 50*(98-105)=-350 -> 99800).
	require.Len(t, res.AccountStates, 4)
	assert.Equal(t, domain.MustMoney("100000"), res.AccountStates[0].BalanceUSD)
	assert.Equal(t, domain.MustMoney("100000"), res.AccountStates[1].BalanceUSD)
	assert.Equal(t, domain.MustMoney("100150"), res.AccountStates[2].BalanceUSD)
	assert.Equal(t, domain.MustMoney("99800"), res.AccountStates[3].BalanceUSD)
}

// TestEquityCurveMarkToMarket checks unrealized PnL appears in the equity curve
// while a position is open. After BUY 100 @105 on bar0, the bar0 sample marks
// at close 105 (unrealized 0); we hold through bar1 (close 108) -> unrealized
// +300 in the total equity sample.
func TestEquityCurveMarkToMarket(t *testing.T) {
	intents := []Intent{
		{Date: ts(2025, 1, 2), Ticker: "AAPL", Side: domain.SideLong, Qty: 100},
	}
	res := runEngine(t, newAAPLConfig(intents), mkBars("AAPL", aaplRows))
	require.Len(t, res.TotalEquityCurve, 5)
	// bar0: filled at 105, marked at 105 -> equity 100000.
	assert.Equal(t, domain.MustMoney("100000"), res.TotalEquityCurve[0].Value)
	// bar1: close 108 -> unrealized 100*(108-105)=300 -> equity 100300.
	assert.Equal(t, domain.MustMoney("100300"), res.TotalEquityCurve[1].Value)
	// bar4: close 98 -> unrealized 100*(98-105)=-700 -> equity 99300.
	assert.Equal(t, domain.MustMoney("99300"), res.TotalEquityCurve[4].Value)
	// strategy equity curve mirrors cumulative PnL (here unrealized only).
	curve := res.StrategyEquity["Scripted-000"]
	require.Len(t, curve, 5)
	assert.Equal(t, domain.MustMoney("300"), curve[1].Value)
}
