package accounting

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// TestSamplerPerStrategyAggregation: two strategies, one realized + one open,
// produce independent per-strategy PnL curves and a combined total equity.
func TestSamplerPerStrategyAggregation(t *testing.T) {
	a := NewAccount(domain.MustMoney("100000"), nil)
	tsA := time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)
	s := NewEquitySampler(a)

	// Strategy A: buy 100 @105, sell 100 @110 -> realized 500 (flat).
	_, _, _ = a.ApplyFill(acctFill("A", "AAPL", domain.OrderSideBuy, 100, "105.00", tsA))
	_, _, _ = a.ApplyFill(acctFill("A", "AAPL", domain.OrderSideSell, 100, "110.00", tsA))
	// Strategy B: buy 50 @200, still open; mark at 210 -> unrealized 500.
	_, _, _ = a.ApplyFill(acctFill("B", "MSFT", domain.OrderSideBuy, 50, "200.00", tsA))
	a.ObserveBar(domain.Bar{Symbol: "MSFT", TS: tsA, Close: domain.MustPrice("210.00")})

	require.NoError(t, s.Sample(tsA))

	curveA := s.StrategyCurve("A")
	curveB := s.StrategyCurve("B")
	require.Len(t, curveA, 1)
	require.Len(t, curveB, 1)
	assert.Equal(t, domain.MustMoney("500"), curveA[0].Value, "A realized 500")
	assert.Equal(t, domain.MustMoney("500"), curveB[0].Value, "B unrealized 500")

	total := s.TotalCurve()
	require.Len(t, total, 1)
	// equity = cash(100500 from A realized) + unrealized(500 from B) = 101000.
	assert.Equal(t, domain.MustMoney("101000"), total[0].Value)

	assert.Equal(t, []string{"A", "B"}, s.StrategyIDs())
}

// TestSamplerMultipleBars accumulates points across sampling calls.
func TestSamplerMultipleBars(t *testing.T) {
	a := NewAccount(domain.MustMoney("100000"), nil)
	s := NewEquitySampler(a)
	t1 := time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)
	t2 := time.Date(2025, 1, 3, 0, 0, 0, 0, time.UTC)
	_, _, _ = a.ApplyFill(acctFill("A", "AAPL", domain.OrderSideBuy, 100, "105.00", t1))
	a.ObserveBar(domain.Bar{Symbol: "AAPL", TS: t1, Close: domain.MustPrice("105.00")})
	require.NoError(t, s.Sample(t1))
	a.ObserveBar(domain.Bar{Symbol: "AAPL", TS: t2, Close: domain.MustPrice("108.00")})
	require.NoError(t, s.Sample(t2))

	curve := s.StrategyCurve("A")
	require.Len(t, curve, 2)
	assert.Equal(t, domain.MustMoney("0"), curve[0].Value)   // marked at entry
	assert.Equal(t, domain.MustMoney("300"), curve[1].Value) // +300 unrealized
}
