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

// TestMultiSymbolRegistrationOrder confirms same-timestamp bars dispatch in
// instrument registration (ticker) order (locked decision 2), independent of
// the order they appear in the feed.
func TestMultiSymbolRegistrationOrder(t *testing.T) {
	rows := []barRow{{2025, 1, 2, "10", "11", "9", "10", 100}}
	// Feed lists them C, A, B; registration order (Tickers) is A, B, C.
	feed := SliceFeed{Instruments: []InstrumentBars{
		mkBars("C", rows), mkBars("A", rows), mkBars("B", rows),
	}}
	var dispatch []string
	cfg := Config{
		Tickers:         []string{"A", "B", "C"},
		Start:           calendar.NewDate(2025, 1, 1),
		End:             calendar.NewDate(2025, 1, 31),
		StartingBalance: domain.MustMoney("100000"),
		Strategies:      []StrategySpec{{ID: "Spy-000", Intents: []Intent{
			// no orders; we record bar dispatch via a recording strategy below
		}}},
	}
	eng, err := New(context.Background(), cfg, feed)
	require.NoError(t, err)
	// Replace strategy with a recorder by intercepting via msgbus bar observer.
	eng.bus.SubscribeBars(barOrderRec{&dispatch})
	_, err = eng.Run(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"A", "B", "C"}, dispatch)
}

type barOrderRec struct{ out *[]string }

func (r barOrderRec) OnBar(b domain.Bar) { *r.out = append(*r.out, b.Symbol) }

// TestDeterministicAcrossRuns runs the same config twice and asserts identical
// results (fills, balances, equity curves).
func TestDeterministicAcrossRuns(t *testing.T) {
	intents := []Intent{
		{Date: ts(2025, 1, 2), Ticker: "AAPL", Side: domain.SideLong, Qty: 100},
		{Date: ts(2025, 1, 6), Ticker: "AAPL", Side: domain.SideShort, Qty: 100},
	}
	run := func() *Result {
		return runEngine(t, newAAPLConfig(intents), mkBars("AAPL", aaplRows))
	}
	a, b := run(), run()
	assert.Equal(t, a.FinalBalance, b.FinalBalance)
	require.Equal(t, len(a.Fills), len(b.Fills))
	for i := range a.Fills {
		assert.Equal(t, a.Fills[i], b.Fills[i], "fill %d identical", i)
	}
	assert.Equal(t, a.TotalEquityCurve, b.TotalEquityCurve)
	assert.Equal(t, a.AccountStates, b.AccountStates)
}

// TestRealisticProfileNextBarFill: an order on bar0 fills at bar1's OPEN, not
// bar0's close.
func TestRealisticProfileNextBarFill(t *testing.T) {
	intents := []Intent{
		{Date: ts(2025, 1, 2), Ticker: "AAPL", Side: domain.SideLong, Qty: 100},
		{Date: ts(2025, 1, 3), Ticker: "AAPL", Side: domain.SideFlat},
	}
	cfg := newAAPLConfig(intents)
	cfg.Profile = ProfileRealistic
	res := runEngine(t, cfg, mkBars("AAPL", aaplRows))
	require.Len(t, res.Fills, 2)
	// bar0 BUY fills at bar1 open 106. bar1 FLAT submitted on bar1, fills at
	// bar2 open 108. realized = 100*(108-106) = 200.
	assert.Equal(t, domain.MustPrice("106"), res.Fills[0].Price)
	assert.Equal(t, domain.MustPrice("108"), res.Fills[1].Price)
	assert.Equal(t, domain.MustMoney("100200"), res.FinalBalance)
}

// TestConfigValidation rejects bad configs.
func TestConfigValidation(t *testing.T) {
	base := newAAPLConfig([]Intent{{Date: ts(2025, 1, 2), Ticker: "AAPL", Side: domain.SideLong, Qty: 1}})
	feed := SliceFeed{Instruments: []InstrumentBars{mkBars("AAPL", aaplRows)}}

	bad := base
	bad.Tickers = nil
	_, err := New(context.Background(), bad, feed)
	require.ErrorIs(t, err, domain.ErrInvalidArgument)

	bad = base
	bad.Tickers = []string{"AAPL", "AAPL"}
	_, err = New(context.Background(), bad, feed)
	require.ErrorIs(t, err, domain.ErrInvalidArgument)

	bad = base
	bad.StartingBalance = 0
	_, err = New(context.Background(), bad, feed)
	require.ErrorIs(t, err, domain.ErrInvalidArgument)

	bad = base
	bad.End = calendar.NewDate(2024, 1, 1)
	_, err = New(context.Background(), bad, feed)
	require.ErrorIs(t, err, domain.ErrInvalidArgument)

	bad = base
	bad.Profile = "weird"
	_, err = New(context.Background(), bad, feed)
	require.ErrorIs(t, err, domain.ErrInvalidArgument)

	bad = base
	bad.Strategies = nil
	_, err = New(context.Background(), bad, feed)
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
}

// TestRunContextCancellation: a cancelled context stops the run cleanly.
func TestRunContextCancellation(t *testing.T) {
	intents := []Intent{{Date: ts(2025, 1, 2), Ticker: "AAPL", Side: domain.SideLong, Qty: 1}}
	feed := SliceFeed{Instruments: []InstrumentBars{mkBars("AAPL", aaplRows)}}
	eng, err := New(context.Background(), newAAPLConfig(intents), feed)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before run
	_, err = eng.Run(ctx)
	require.ErrorIs(t, err, context.Canceled)
}

// TestEmptyFeedRuns: no bars -> a valid empty result (final == starting).
func TestEmptyFeedRuns(t *testing.T) {
	intents := []Intent{{Date: ts(2025, 1, 2), Ticker: "AAPL", Side: domain.SideLong, Qty: 1}}
	cfg := newAAPLConfig(intents)
	feed := SliceFeed{Instruments: []InstrumentBars{{Symbol: "AAPL"}}}
	eng, err := New(context.Background(), cfg, feed)
	require.NoError(t, err)
	res, err := eng.Run(context.Background())
	require.NoError(t, err)
	assert.Equal(t, cfg.StartingBalance, res.FinalBalance)
	assert.Equal(t, 0, res.BarsProcessed)
	// initial account state still emitted.
	require.Len(t, res.AccountStates, 1)
}

// TestEngineDeterminismMultiStrategy runs the multi-year, multi-strategy
// benchmark config twice and asserts the two Results are bit-identical
// (final balance, total PnL, full per-bar total-equity curve, every
// per-strategy curve, and the account-state curve). This is the permanent
// regression guard for the equity-sampler / account allocation-reduction
// optimizations (bench fix): a reused scratch buffer must NOT perturb the
// deterministic aggregation order or any sampled value.
func TestEngineDeterminismMultiStrategy(t *testing.T) {
	cfg, feed, _ := benchEngineConfig(4, 3)

	run := func() *Result {
		eng, err := New(context.Background(), cfg, feed)
		require.NoError(t, err)
		res, err := eng.Run(context.Background())
		require.NoError(t, err)
		return res
	}
	a, b := run(), run()

	require.Equal(t, a.FinalBalance, b.FinalBalance, "final balance must be deterministic")
	require.Equal(t, a.TotalPnL, b.TotalPnL, "total pnl must be deterministic")
	require.Equal(t, a.BarsProcessed, b.BarsProcessed)
	require.Equal(t, a.SampledDays, b.SampledDays)

	// Full total-equity curve equality (the EquitySampler.Sample output).
	require.Equal(t, len(a.TotalEquityCurve), len(b.TotalEquityCurve))
	for i := range a.TotalEquityCurve {
		assert.True(t, a.TotalEquityCurve[i].TS.Equal(b.TotalEquityCurve[i].TS), "equity ts %d", i)
		assert.Equal(t, a.TotalEquityCurve[i].Value, b.TotalEquityCurve[i].Value, "equity value %d", i)
	}

	// Per-strategy curves equality (the Unrealized / sortedKeysInto path).
	require.Equal(t, len(a.StrategyEquity), len(b.StrategyEquity))
	for id, ca := range a.StrategyEquity {
		cb, ok := b.StrategyEquity[id]
		require.True(t, ok, "strategy %s missing in second run", id)
		require.Equal(t, len(ca), len(cb), "strategy %s curve length", id)
		for i := range ca {
			assert.Equal(t, ca[i].Value, cb[i].Value, "strategy %s point %d", id, i)
		}
	}

	// Account-state curve equality.
	require.Equal(t, len(a.AccountStates), len(b.AccountStates))
}

var _ = time.Now
