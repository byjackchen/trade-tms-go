package accounting

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/core"
	"github.com/byjackchen/trade-tms-go/internal/domain"
)

func acctFill(strat, sym string, side domain.OrderSide, qty int64, px string, ts time.Time) domain.Fill {
	return domain.Fill{
		TradeID: "t-" + sym, ClientOrderID: "o", StrategyID: strat, Symbol: sym,
		Side: side, Qty: domain.Qty(qty), Price: domain.MustPrice(px), TS: ts,
	}
}

func TestAccountCashTracksRealized(t *testing.T) {
	bus := core.NewMsgBus()
	a := NewAccount(domain.MustMoney("100000"), bus)
	ts := time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)
	_, _, err := a.ApplyFill(acctFill("S", "AAPL", domain.OrderSideBuy, 100, "105.00", ts))
	require.NoError(t, err)
	cash, _ := a.Cash()
	assert.Equal(t, domain.MustMoney("100000"), cash) // open: no realized
	_, _, err = a.ApplyFill(acctFill("S", "AAPL", domain.OrderSideSell, 100, "110.00", ts))
	require.NoError(t, err)
	cash, _ = a.Cash()
	assert.Equal(t, domain.MustMoney("100500"), cash)
}

func TestAccountEquityIncludesUnrealized(t *testing.T) {
	a := NewAccount(domain.MustMoney("100000"), nil)
	ts := time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)
	_, _, err := a.ApplyFill(acctFill("S", "AAPL", domain.OrderSideBuy, 100, "105.00", ts))
	require.NoError(t, err)
	// mark at 108.
	a.ObserveBar(domain.Bar{Symbol: "AAPL", TS: ts, Close: domain.MustPrice("108.00")})
	eq, err := a.Equity()
	require.NoError(t, err)
	assert.Equal(t, domain.MustMoney("100300"), eq)
}

func TestAccountMarginSupportsShort(t *testing.T) {
	a := NewAccount(domain.MustMoney("100000"), nil)
	ts := time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)
	// Open short directly (margin account).
	_, out, err := a.ApplyFill(acctFill("S", "AAPL", domain.OrderSideSell, 100, "105.00", ts))
	require.NoError(t, err)
	assert.True(t, out.Opened)
	pos, ok := a.Position("S", "AAPL")
	require.True(t, ok)
	assert.Equal(t, domain.Qty(-100), pos.SignedQty)
}

func TestAccountNettingTwoStrategiesSameSymbol(t *testing.T) {
	a := NewAccount(domain.MustMoney("100000"), nil)
	ts := time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)
	_, _, err := a.ApplyFill(acctFill("A", "AAPL", domain.OrderSideBuy, 100, "105.00", ts))
	require.NoError(t, err)
	_, _, err = a.ApplyFill(acctFill("B", "AAPL", domain.OrderSideSell, 40, "105.00", ts))
	require.NoError(t, err)
	// Two separate positions exist.
	pa, _ := a.Position("A", "AAPL")
	pb, _ := a.Position("B", "AAPL")
	assert.Equal(t, domain.Qty(100), pa.SignedQty)
	assert.Equal(t, domain.Qty(-40), pb.SignedQty)
	// Cross-strategy net for risk snapshot.
	snap, err := a.Snapshot()
	require.NoError(t, err)
	net, err := snap.NetPositionAcrossStrategies("AAPL")
	require.NoError(t, err)
	assert.Equal(t, domain.Qty(60), net)
}

func TestAccountEmitsStateOnEachFill(t *testing.T) {
	bus := core.NewMsgBus()
	var states []domain.Money
	bus.SubscribeAccountState(stateRec{&states})
	a := NewAccount(domain.MustMoney("100000"), bus)
	ts := time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)
	require.NoError(t, a.EmitInitialState(ts))
	_, _, _ = a.ApplyFill(acctFill("S", "AAPL", domain.OrderSideBuy, 100, "105.00", ts))
	_, _, _ = a.ApplyFill(acctFill("S", "AAPL", domain.OrderSideSell, 100, "110.00", ts))
	// initial + 2 fills.
	require.Len(t, states, 3)
	assert.Equal(t, domain.MustMoney("100000"), states[0])
	assert.Equal(t, domain.MustMoney("100000"), states[1])
	assert.Equal(t, domain.MustMoney("100500"), states[2])
}

func TestAccountSnapshotSkipsFlat(t *testing.T) {
	a := NewAccount(domain.MustMoney("100000"), nil)
	ts := time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)
	_, _, _ = a.ApplyFill(acctFill("S", "AAPL", domain.OrderSideBuy, 100, "105.00", ts))
	_, _, _ = a.ApplyFill(acctFill("S", "AAPL", domain.OrderSideSell, 100, "110.00", ts))
	snap, err := a.Snapshot()
	require.NoError(t, err)
	// Flat position is excluded from the snapshot positions map (reference glue).
	assert.Empty(t, snap.Positions)
}

type stateRec struct{ out *[]domain.Money }

func (r stateRec) OnAccountState(s core.AccountState) { *r.out = append(*r.out, s.Total) }
