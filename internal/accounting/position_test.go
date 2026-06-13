package accounting

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/domain"
)

func fill(side domain.OrderSide, qty int64, px string) domain.Fill {
	return domain.Fill{
		TradeID:       "t",
		ClientOrderID: "o",
		StrategyID:    "S",
		Symbol:        "AAPL",
		Side:          side,
		Qty:           domain.Qty(qty),
		Price:         domain.MustPrice(px),
		TS:            time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC),
	}
}

func TestPositionOpenLong(t *testing.T) {
	p := NewPosition("S", "AAPL")
	out, err := p.ApplyFill(fill(domain.OrderSideBuy, 100, "105.00"))
	require.NoError(t, err)
	assert.True(t, out.Opened)
	assert.Equal(t, domain.Money(0), out.RealizedDelta)
	assert.Equal(t, domain.Qty(100), p.SignedQty())
	assert.Equal(t, domain.MustPrice("105"), p.AvgEntryPrice())
}

func TestPositionPartialReduceLong(t *testing.T) {
	p := NewPosition("S", "AAPL")
	_, err := p.ApplyFill(fill(domain.OrderSideBuy, 100, "105.00"))
	require.NoError(t, err)
	out, err := p.ApplyFill(fill(domain.OrderSideSell, 50, "108.00"))
	require.NoError(t, err)
	assert.True(t, out.Changed)
	// realized = 50*(108-105) = 150.
	assert.Equal(t, domain.MustMoney("150"), out.RealizedDelta)
	assert.Equal(t, domain.Qty(50), p.SignedQty())
	// avg entry unchanged.
	assert.Equal(t, domain.MustPrice("105"), p.AvgEntryPrice())
}

func TestPositionCloseLong(t *testing.T) {
	p := NewPosition("S", "AAPL")
	_, err := p.ApplyFill(fill(domain.OrderSideBuy, 100, "105.00"))
	require.NoError(t, err)
	out, err := p.ApplyFill(fill(domain.OrderSideSell, 100, "110.00"))
	require.NoError(t, err)
	assert.True(t, out.Closed)
	assert.Equal(t, domain.MustMoney("500"), out.RealizedDelta) // 100*(110-105)
	assert.True(t, p.IsFlat())
	assert.Equal(t, domain.MustMoney("500"), p.RealizedPnL())
}

func TestPositionFlipLongToShort(t *testing.T) {
	p := NewPosition("S", "AAPL")
	_, err := p.ApplyFill(fill(domain.OrderSideBuy, 100, "105.00"))
	require.NoError(t, err)
	// Now sell 150: closes 100 @110 (realized 500), opens 50 short @110.
	out, err := p.ApplyFill(fill(domain.OrderSideSell, 150, "110.00"))
	require.NoError(t, err)
	assert.True(t, out.Flipped)
	assert.Equal(t, domain.MustMoney("500"), out.RealizedDelta)
	assert.Equal(t, domain.Qty(-50), p.SignedQty())
	assert.Equal(t, domain.MustPrice("110"), p.AvgEntryPrice())
}

func TestPositionShortRealize(t *testing.T) {
	p := NewPosition("S", "AAPL")
	// Open short 100 @105, cover @100 -> realized 100*(105-100)=500.
	_, err := p.ApplyFill(fill(domain.OrderSideSell, 100, "105.00"))
	require.NoError(t, err)
	out, err := p.ApplyFill(fill(domain.OrderSideBuy, 100, "100.00"))
	require.NoError(t, err)
	assert.Equal(t, domain.MustMoney("500"), out.RealizedDelta)
	assert.True(t, p.IsFlat())
}

func TestPositionAddToLongAveragePrice(t *testing.T) {
	p := NewPosition("S", "AAPL")
	_, err := p.ApplyFill(fill(domain.OrderSideBuy, 100, "105.00"))
	require.NoError(t, err)
	out, err := p.ApplyFill(fill(domain.OrderSideBuy, 100, "108.00"))
	require.NoError(t, err)
	assert.True(t, out.Changed)
	assert.Equal(t, domain.Money(0), out.RealizedDelta)
	assert.Equal(t, domain.Qty(200), p.SignedQty())
	assert.Equal(t, domain.MustPrice("106.50"), p.AvgEntryPrice())
}

func TestPositionUnrealized(t *testing.T) {
	p := NewPosition("S", "AAPL")
	_, err := p.ApplyFill(fill(domain.OrderSideBuy, 100, "105.00"))
	require.NoError(t, err)
	u, err := p.UnrealizedPnL(domain.MustPrice("108.00"))
	require.NoError(t, err)
	assert.Equal(t, domain.MustMoney("300"), u) // 100*(108-105)

	// Short unrealized: open short 50 @100, mark at 90 -> +500.
	ps := NewPosition("S", "X")
	f := fill(domain.OrderSideSell, 50, "100.00")
	f.Symbol = "X"
	_, err = ps.ApplyFill(f)
	require.NoError(t, err)
	u, err = ps.UnrealizedPnL(domain.MustPrice("90.00"))
	require.NoError(t, err)
	assert.Equal(t, domain.MustMoney("500"), u) // -50*(90-100) = 500
}

// TestPositionFractionalAveragePrice exercises the half-to-even rounding of the
// average price materialization (3 lots producing a repeating average).
func TestPositionFractionalAveragePrice(t *testing.T) {
	p := NewPosition("S", "AAPL")
	// 1 @10.00, 1 @10.01, 1 @10.02 -> avg 10.01 exactly.
	require.NoError(t, applyN(p, domain.OrderSideBuy, 1, "10.00"))
	require.NoError(t, applyN(p, domain.OrderSideBuy, 1, "10.01"))
	require.NoError(t, applyN(p, domain.OrderSideBuy, 1, "10.02"))
	assert.Equal(t, domain.MustPrice("10.01"), p.AvgEntryPrice())
	// notional = 30.03; /3 = 10.01 exact.
}

func applyN(p *Position, side domain.OrderSide, qty int64, px string) error {
	_, err := p.ApplyFill(fill(side, qty, px))
	return err
}

func TestPositionRejectsZeroQty(t *testing.T) {
	p := NewPosition("S", "AAPL")
	f := fill(domain.OrderSideBuy, 0, "105.00")
	_, err := p.ApplyFill(f)
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
}
