package exec

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/domain"
)

func bar(sym, o, h, l, c string) domain.Bar {
	return domain.Bar{
		Symbol: sym, TS: time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC),
		Open: domain.MustPrice(o), High: domain.MustPrice(h),
		Low: domain.MustPrice(l), Close: domain.MustPrice(c), Volume: 1000,
	}
}

func order(sym string, side domain.OrderSide, qty int64) domain.Order {
	return domain.NewMarketOrder("O-1", "S", sym, side, domain.Qty(qty), "r",
		time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC))
}

func legPrice(t *testing.T, legs []FillLeg) domain.Price {
	t.Helper()
	require.Len(t, legs, 1)
	return legs[0].Price
}

func TestNautilusCompatFillsAtClose(t *testing.T) {
	m := NautilusCompatModel{}
	assert.Equal(t, FillThisBar, m.Timing())
	// qty 100 <= close-tick (1000/4=250) -> single leg at close 105.
	legs, err := m.Fill(order("AAPL", domain.OrderSideBuy, 100), bar("AAPL", "100", "110", "95", "105"))
	require.NoError(t, err)
	assert.Equal(t, domain.MustPrice("105"), legPrice(t, legs))
	c, err := m.Commission(domain.Qty(100), domain.MustPrice("105"))
	require.NoError(t, err)
	assert.Equal(t, domain.Money(0), c)
}

// TestNautilusCompatDepthWalk reproduces the volume-decomposition fill: an
// order exceeding the close-tick depth fills the depth at close and the
// residual one increment adverse.
func TestNautilusCompatDepthWalk(t *testing.T) {
	m := NautilusCompatModel{}
	b := bar("AAPL", "100", "110", "95", "105")
	b.Volume = 1000 // close tick = 1000 - 3*250 = 250
	// BUY 300 -> 250 @105.00, 50 @105.01.
	legs, err := m.Fill(order("AAPL", domain.OrderSideBuy, 300), b)
	require.NoError(t, err)
	require.Len(t, legs, 2)
	assert.Equal(t, FillLeg{Qty: 250, Price: domain.MustPrice("105.00")}, legs[0])
	assert.Equal(t, FillLeg{Qty: 50, Price: domain.MustPrice("105.01")}, legs[1])
	// SELL 300 -> 250 @105.00, 50 @104.99 (residual walks DOWN).
	legs, err = m.Fill(order("AAPL", domain.OrderSideSell, 300), b)
	require.NoError(t, err)
	require.Len(t, legs, 2)
	assert.Equal(t, domain.MustPrice("104.99"), legs[1].Price)
	// Exactly at close-tick depth -> single leg.
	legs, err = m.Fill(order("AAPL", domain.OrderSideBuy, 250), b)
	require.NoError(t, err)
	assert.Len(t, legs, 1)
}

func TestCloseTickVolume(t *testing.T) {
	// Large volumes: quarter = vol//4, close = vol - 3*quarter.
	assert.Equal(t, int64(250), closeTickVolume(1000))
	assert.Equal(t, int64(251), closeTickVolume(1001))
	assert.Equal(t, int64(25), closeTickVolume(100))
	assert.Equal(t, int64(2), closeTickVolume(8))
	assert.Equal(t, int64(4), closeTickVolume(7))
	// Small volumes hit the min-size floor + underflow guard of
	// compute_bar_quarter_sizes (verified empirically against the Nautilus
	// BacktestEngine — NOT the naive vol-3*floor(vol/4), which would wrongly
	// give 3/2 here; see docs/spec/engine-fill-model.md §3.1).
	assert.Equal(t, int64(1), closeTickVolume(3)) // quarter floors to 1; 3*1>=3 -> 1
	assert.Equal(t, int64(1), closeTickVolume(2)) // quarter floors to 1; 3*1>=2 -> 1
	assert.Equal(t, int64(1), closeTickVolume(1)) // quarter floors to 1; 3*1>=1 -> 1
	assert.Equal(t, int64(0), closeTickVolume(0)) // degenerate zero-volume bar
}

func TestRealisticNextBarOpenWithSlippage(t *testing.T) {
	m := RealisticModel{SlippageBps: 10} // 0.1%
	assert.Equal(t, FillNextBar, m.Timing())
	// BUY pays more: 100 * (1 + 0.001) = 100.10.
	legs, err := m.Fill(order("AAPL", domain.OrderSideBuy, 100), bar("AAPL", "100.00", "110", "95", "105"))
	require.NoError(t, err)
	assert.Equal(t, domain.MustPrice("100.10"), legPrice(t, legs))
	// SELL receives less: 100 * (1 - 0.001) = 99.90.
	legs, err = m.Fill(order("AAPL", domain.OrderSideSell, 100), bar("AAPL", "100.00", "110", "95", "105"))
	require.NoError(t, err)
	assert.Equal(t, domain.MustPrice("99.90"), legPrice(t, legs))
}

func TestRealisticCommission(t *testing.T) {
	// per-share
	m := RealisticModel{CommissionPerShare: domain.MustMoney("0.0050")}
	c, err := m.Commission(domain.Qty(100), domain.MustPrice("105.00"))
	require.NoError(t, err)
	assert.Equal(t, domain.MustMoney("0.50"), c)
	// bps of notional: 5 bps of 100*105 = 10500 -> 5.25.
	mb := RealisticModel{CommissionBps: 5}
	c, err = mb.Commission(domain.Qty(100), domain.MustPrice("105.00"))
	require.NoError(t, err)
	assert.Equal(t, domain.MustMoney("5.25"), c)
}

// fakeSink and fakeSeq drive the executor in isolation.
type fakeSink struct{ fills []domain.Fill }

func (s *fakeSink) EmitFill(f domain.Fill) error { s.fills = append(s.fills, f); return nil }

type fakeSeq struct{ n uint64 }

func (s *fakeSeq) NextSeq() uint64 { v := s.n; s.n++; return v }

func TestExecutorThisBarFlow(t *testing.T) {
	sink := &fakeSink{}
	ex := NewSimExecutor(NautilusCompatModel{}, sink, &fakeSeq{})
	o := order("AAPL", domain.OrderSideBuy, 100)
	require.NoError(t, ex.Submit(o))
	// This-bar model: ProcessBar RECORDS the bar (no fill yet); the order is
	// filled by the end-of-timestamp FlushThisBar against the order's own
	// symbol's recorded bar. This two-phase flow lets a strategy submit an order
	// for symbol X while symbol Y's bar dispatches and still fill X at THIS
	// timestamp's close (cross-symbol same-ts fill, Nautilus parity).
	n, err := ex.ProcessBar(bar("AAPL", "100", "110", "95", "105"))
	require.NoError(t, err)
	assert.Equal(t, 0, n, "ProcessBar records (no fill) in the this-bar model")
	assert.Len(t, sink.fills, 0)
	assert.Equal(t, 1, ex.PendingCount(), "order awaits the end-of-ts flush")

	n, err = ex.FlushThisBar()
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	require.Len(t, sink.fills, 1)
	assert.Equal(t, domain.MustPrice("105"), sink.fills[0].Price)
	assert.Equal(t, 0, ex.PendingCount())
}

// TestExecutorThisBarCrossSymbol proves a market order submitted for symbol X
// while symbol Y's bar is the one driving ProcessBar still fills at X's close
// for THIS timestamp (both bars recorded before the flush). This is the
// multi-leg (Pairs) case that the per-symbol ProcessBar fill model got wrong
// (X would have filled one bar late).
func TestExecutorThisBarCrossSymbol(t *testing.T) {
	sink := &fakeSink{}
	ex := NewSimExecutor(NautilusCompatModel{}, sink, &fakeSeq{})
	// Both legs' bars at the same timestamp are recorded (registration order),
	// then both leg orders are submitted (e.g. during the 2nd leg's dispatch).
	if _, err := ex.ProcessBar(bar("KO", "53", "54", "52", "53.15")); err != nil {
		t.Fatal(err)
	}
	if _, err := ex.ProcessBar(bar("PEP", "142", "143", "141", "142.54")); err != nil {
		t.Fatal(err)
	}
	require.NoError(t, ex.Submit(order("KO", domain.OrderSideSell, 282)))
	require.NoError(t, ex.Submit(order("PEP", domain.OrderSideBuy, 105)))
	n, err := ex.FlushThisBar()
	require.NoError(t, err)
	assert.Equal(t, 2, n, "both orders filled at this ts")
	assert.Equal(t, 0, ex.PendingCount())
	// Each order fills against ITS OWN symbol's bar this ts: every KO fill leg
	// prices off KO's bar [52,54], every PEP leg off PEP's bar [141,143].
	koFilled, pepFilled := false, false
	for _, f := range sink.fills {
		switch f.Symbol {
		case "KO":
			koFilled = true
			assert.True(t, f.Price.Cmp(domain.MustPrice("52")) >= 0 && f.Price.Cmp(domain.MustPrice("54")) <= 0,
				"KO fill %s within KO bar range", f.Price)
		case "PEP":
			pepFilled = true
			assert.True(t, f.Price.Cmp(domain.MustPrice("141")) >= 0 && f.Price.Cmp(domain.MustPrice("143")) <= 0,
				"PEP fill %s within PEP bar range", f.Price)
		default:
			t.Fatalf("unexpected fill symbol %s", f.Symbol)
		}
	}
	assert.True(t, koFilled && pepFilled, "both legs filled this ts")
}

func TestExecutorNextBarFlow(t *testing.T) {
	sink := &fakeSink{}
	ex := NewSimExecutor(RealisticModel{}, sink, &fakeSeq{})
	require.NoError(t, ex.Submit(order("AAPL", domain.OrderSideBuy, 100)))
	// Same-symbol bar fills the pending order at its open.
	n, err := ex.ProcessBar(bar("AAPL", "101.00", "110", "95", "105"))
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	require.Len(t, sink.fills, 1)
	assert.Equal(t, domain.MustPrice("101"), sink.fills[0].Price)
}

func TestExecutorRejectsNonMarket(t *testing.T) {
	ex := NewSimExecutor(NautilusCompatModel{}, &fakeSink{}, &fakeSeq{})
	o := order("AAPL", domain.OrderSideBuy, 100)
	o.Type = domain.OrderTypeLimit
	lp := domain.MustPrice("100")
	o.LimitPrice = &lp
	err := ex.Submit(o)
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
}

func TestExecutorDeterministicIDs(t *testing.T) {
	ex := NewSimExecutor(NautilusCompatModel{}, &fakeSink{}, &fakeSeq{})
	assert.Equal(t, "O-0", ex.NewClientOrderID())
	assert.Equal(t, "O-1", ex.NewClientOrderID())
}
