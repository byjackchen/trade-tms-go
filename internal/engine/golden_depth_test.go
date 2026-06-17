package engine

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// TestGoldenDepthWalk pins the depth-walk scenario where a BUY 300 against a
// 1000-volume bar fills 250 @105.00 + 50 @105.01 (depth walk), so the average
// entry is 105.001666..., and the subsequent reduces realize a final balance of
// 98599.49 (realized -1400.51). This exercises the volume-decomposition fill
// model AND the exact-notional realized-PnL math; the values are this repo's
// golden regression baseline.
func TestGoldenDepthWalk(t *testing.T) {
	intents := []Intent{
		{Date: ts(2025, 1, 2), Ticker: "AAPL", Side: domain.SideLong, Qty: 300},
		{Date: ts(2025, 1, 3), Ticker: "AAPL", Side: domain.SideShort, Qty: 100},
		{Date: ts(2025, 1, 6), Ticker: "AAPL", Side: domain.SideShort, Qty: 100},
		{Date: ts(2025, 1, 7), Ticker: "AAPL", Side: domain.SideShort, Qty: 100},
	}
	res := runEngine(t, newAAPLConfig(intents), mkBars("AAPL", aaplRows))

	// Bar0 BUY 300 fills in two legs.
	require.GreaterOrEqual(t, len(res.Fills), 5)
	assert.Equal(t, domain.MustPrice("105.00"), res.Fills[0].Price)
	assert.Equal(t, domain.Qty(250), res.Fills[0].Qty)
	assert.Equal(t, domain.MustPrice("105.01"), res.Fills[1].Price)
	assert.Equal(t, domain.Qty(50), res.Fills[1].Qty)

	// Final balance is pinned exactly.
	assert.Equal(t, domain.MustMoney("98599.49"), res.FinalBalance,
		"depth-walk realized PnL must match golden baseline 98599.49")
}
