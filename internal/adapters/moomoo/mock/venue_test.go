package mock_test

// venue_test.go drives the native Go TRADING client against the mock TRADING
// venue end to end over a real TCP socket: account discovery, funds, an order
// that is accepted then filled at the next pushed bar (with Trd_UpdateOrder +
// Trd_UpdateOrderFill pushes), the resulting broker position, idempotent
// re-submission, and the three deterministic reject paths (unknown symbol,
// insufficient buying power, market closed). It is the deterministic P6 trading
// gate: green here predicts green against a real account.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	mo "github.com/byjackchen/trade-tms-go/internal/adapters/moomoo"
	"github.com/byjackchen/trade-tms-go/internal/adapters/moomoo/mock"
	"github.com/byjackchen/trade-tms-go/internal/adapters/moomoo/pb/qotcommon"
	"github.com/byjackchen/trade-tms-go/internal/domain"
)

const (
	paperAccID = uint64(1001)
	liveAccID  = uint64(9001)
)

// startTradingMock spins up a mock OpenD with the trading venue enabled.
func startTradingMock(t *testing.T, src mock.BarSource, cfg mock.VenueConfig) *mock.Server {
	t.Helper()
	srv, err := mock.New(mock.Options{
		Listen:            "127.0.0.1:0",
		Source:            src,
		KeepAliveInterval: 1,
		Now:               func() time.Time { return time.Date(2024, 6, 13, 14, 0, 0, 0, time.UTC) },
	})
	require.NoError(t, err)
	srv.EnableTrading(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		_ = srv.Close()
	})
	return srv
}

// pushCollector accumulates trading pushes from the client handlers.
type pushCollector struct {
	mu     sync.Mutex
	orders []mo.OrderUpdate
	fills  []mo.FillUpdate
}

func (p *pushCollector) order(u mo.OrderUpdate) {
	p.mu.Lock()
	p.orders = append(p.orders, u)
	p.mu.Unlock()
}

func (p *pushCollector) fill(f mo.FillUpdate) {
	p.mu.Lock()
	p.fills = append(p.fills, f)
	p.mu.Unlock()
}

func (p *pushCollector) snapshot() ([]mo.OrderUpdate, []mo.FillUpdate) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]mo.OrderUpdate(nil), p.orders...), append([]mo.FillUpdate(nil), p.fills...)
}

// connectTradingClient builds a client wired with the push collector and waits
// for it to be ready.
func connectTradingClient(t *testing.T, addr string, pc *pushCollector) *mo.Client {
	t.Helper()
	c := mo.NewClient(mo.Options{
		Addr:           addr,
		RequestTimeout: 3 * time.Second,
		OnTrdOrder:     pc.order,
		OnTrdOrderFill: pc.fill,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c.Start(context.Background())
	require.NoError(t, c.Ready(ctx))
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestVenueAccountsAndFunds(t *testing.T) {
	src := mock.NewMemBarSource()
	srv := startTradingMock(t, src, mock.VenueConfig{PaperAccID: paperAccID, LiveAccID: liveAccID, StartingPower: 50_000})
	pc := &pushCollector{}
	c := connectTradingClient(t, srv.Addr(), pc)
	ctx := context.Background()

	paper, err := c.GetAccList(ctx, mo.TrdEnvSimulate)
	require.NoError(t, err)
	require.Len(t, paper, 1)
	require.Equal(t, paperAccID, paper[0].AccID)
	require.Equal(t, mo.TrdEnvSimulate, paper[0].TrdEnv)

	live, err := c.GetAccList(ctx, mo.TrdEnvReal)
	require.NoError(t, err)
	require.Len(t, live, 1)
	require.Equal(t, liveAccID, live[0].AccID)

	funds, err := c.GetFunds(ctx, paperAccID, mo.TrdEnvSimulate)
	require.NoError(t, err)
	require.Equal(t, "50000", funds.AvailableFunds.String())
}

func TestVenuePlaceOrderFillsAtNextBar(t *testing.T) {
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	bars := fixtureBars(t, "AAPL", start, 5)
	src := mock.NewMemBarSource()
	src.Add("AAPL", qotcommon.KLType_KLType_Day, bars)
	srv := startTradingMock(t, src, mock.VenueConfig{PaperAccID: paperAccID, StartingPower: 1_000_000})
	pc := &pushCollector{}
	c := connectTradingClient(t, srv.Addr(), pc)
	ctx := context.Background()

	// Subscribe so the venue's PushKLine reaches this connection (and drives the
	// fill on the next bar).
	require.NoError(t, c.Subscribe(ctx, []string{"AAPL"}, qotcommon.KLType_KLType_Day))

	// Place a BUY 100 AAPL market order. It is accepted (Submitted push) but not
	// yet filled.
	res, err := c.PlaceOrder(ctx, mo.PlaceOrderRequest{
		AccID:         paperAccID,
		TrdEnv:        mo.TrdEnvSimulate,
		ClientOrderID: "O-1",
		Symbol:        "AAPL",
		Side:          domain.OrderSideBuy,
		Type:          domain.OrderTypeMarket,
		TIF:           domain.TIFGTC,
		Qty:           100,
	})
	require.NoError(t, err)
	require.NotEmpty(t, res.VenueOrderID)

	requireEventually(t, func() bool {
		orders, _ := pc.snapshot()
		for _, o := range orders {
			if o.ClientOrderID == "O-1" && o.Class() == mo.StatusClassAccepted {
				return true
			}
		}
		return false
	}, "expected an ACCEPTED (Submitted) push for O-1")

	// Now push the NEXT bar — the documented fill trigger. The order fills at that
	// bar's close.
	fillBar := bars[2]
	_, err = srv.PushKLine("AAPL", qotcommon.KLType_KLType_Day, []domain.Bar{fillBar})
	require.NoError(t, err)

	requireEventually(t, func() bool {
		orders, fills := pc.snapshot()
		var filled, gotFill bool
		for _, o := range orders {
			if o.ClientOrderID == "O-1" && o.IsFullFill() {
				filled = true
			}
		}
		for _, f := range fills {
			if f.VenueOrderID == res.VenueOrderID && f.Qty == 100 {
				gotFill = true
			}
		}
		return filled && gotFill
	}, "expected a FILLED order push + a fill push for O-1")

	// The fill price is the next bar's close.
	_, fills := pc.snapshot()
	var found bool
	for _, f := range fills {
		if f.VenueOrderID == res.VenueOrderID {
			require.Equal(t, fillBar.Close.String(), f.Price.String())
			found = true
		}
	}
	require.True(t, found)

	// The broker position now reflects the fill: long 100 AAPL.
	pos, err := c.GetPositionList(ctx, paperAccID, mo.TrdEnvSimulate)
	require.NoError(t, err)
	require.Len(t, pos, 1)
	require.Equal(t, "AAPL", pos[0].Symbol)
	require.EqualValues(t, 100, pos[0].Qty)

	// And the mock venue's own books agree (cross-check the wire view).
	vp := srv.VenuePositions(paperAccID)
	require.Len(t, vp, 1)
	require.EqualValues(t, 100, vp[0].Qty)

	// DIRECTION-2 sync reads: after the fill the order list reports the order as
	// FILLED (history retained) and the FILL list returns the execution — the reads
	// the manual desk's SyncFromBroker depends on. (Previously the mock did not
	// implement Trd_GetOrderFillList, so a sync timed out.)
	ol, err := c.GetOrderList(ctx, paperAccID, mo.TrdEnvSimulate)
	require.NoError(t, err)
	var sawFilled bool
	for _, o := range ol {
		if o.ClientOrderID == "O-1" && o.IsFullFill() {
			sawFilled = true
		}
	}
	require.True(t, sawFilled, "the filled order is retained in Trd_GetOrderList history")

	fl, err := c.GetOrderFillList(ctx, paperAccID, mo.TrdEnvSimulate)
	require.NoError(t, err)
	require.Len(t, fl, 1, "Trd_GetOrderFillList returns the one execution")
	require.Equal(t, "AAPL", fl[0].Symbol)
	require.EqualValues(t, 100, fl[0].Qty)
	require.Equal(t, fillBar.Close.String(), fl[0].Price.String())
	require.Equal(t, res.VenueOrderID, fl[0].VenueOrderID)
}

// TestVenueAutoFillNoPushKLine proves the AUTONOMOUS fill driver fills a working
// order WITHOUT any PushKLine tick — the exact gap that made the STANDALONE
// mock-opend leave every manual order stuck ACCEPTED (findings 1 & 2). It places a
// BUY and asserts it reaches FILLED, opens a broker position, and surfaces in the
// DIRECTION-2 sync reads (order list FILLED + fill list) — all on the wall-clock
// driver alone, with NO controllable-clock K-line push.
func TestVenueAutoFillNoPushKLine(t *testing.T) {
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	bars := fixtureBars(t, "AAPL", start, 5)
	src := mock.NewMemBarSource()
	src.Add("AAPL", qotcommon.KLType_KLType_Day, bars)

	srv, err := mock.New(mock.Options{
		Listen:            "127.0.0.1:0",
		Source:            src,
		KeepAliveInterval: 1,
		Now:               func() time.Time { return time.Date(2024, 6, 13, 14, 0, 0, 0, time.UTC) },
	})
	require.NoError(t, err)
	srv.EnableTrading(mock.VenueConfig{PaperAccID: paperAccID, StartingPower: 1_000_000})
	ctx, cancel := context.WithCancel(context.Background())
	// The autonomous driver: a fast tick so the test converges quickly. This is the
	// driver wired into cmd/tms mock-opend (with the default 500ms tick).
	srv.StartAutoFill(ctx, 20*time.Millisecond)
	go func() { _ = srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		_ = srv.Close()
	})

	pc := &pushCollector{}
	c := connectTradingClient(t, srv.Addr(), pc)

	// CRUCIAL: NO Subscribe + NO PushKLine. The only thing that can fill this order
	// is the autonomous driver. (A real live node WOULD subscribe for quotes, but the
	// fill must not DEPEND on a K-line push reaching the venue.)
	res, err := c.PlaceOrder(ctx, mo.PlaceOrderRequest{
		AccID:         paperAccID,
		TrdEnv:        mo.TrdEnvSimulate,
		ClientOrderID: "AUTO-1",
		Symbol:        "AAPL",
		Side:          domain.OrderSideBuy,
		Type:          domain.OrderTypeMarket,
		TIF:           domain.TIFGTC,
		Qty:           10,
	})
	require.NoError(t, err)
	require.NotEmpty(t, res.VenueOrderID)

	requireEventually(t, func() bool {
		orders, fills := pc.snapshot()
		var filled, gotFill bool
		for _, o := range orders {
			if o.ClientOrderID == "AUTO-1" && o.IsFullFill() {
				filled = true
			}
		}
		for _, f := range fills {
			if f.VenueOrderID == res.VenueOrderID && f.Qty == 10 {
				gotFill = true
			}
		}
		return filled && gotFill
	}, "auto-fill driver must FILL the order with no PushKLine")

	// The fill price is the bar source's latest daily close.
	wantPx := bars[len(bars)-1].Close.String()
	_, fills := pc.snapshot()
	var found bool
	for _, f := range fills {
		if f.VenueOrderID == res.VenueOrderID {
			require.Equal(t, wantPx, f.Price.String())
			found = true
		}
	}
	require.True(t, found)

	// Broker position reflects the fill (long 10 AAPL).
	pos, err := c.GetPositionList(ctx, paperAccID, mo.TrdEnvSimulate)
	require.NoError(t, err)
	require.Len(t, pos, 1)
	require.Equal(t, "AAPL", pos[0].Symbol)
	require.EqualValues(t, 10, pos[0].Qty)

	// DIRECTION-2 sync reads (the operator-traded-in-moomoo case): the order list
	// reports FILLED and the fill list returns the execution — all created by the
	// autonomous driver, so a sync against the standalone venue reflects a real fill.
	ol, err := c.GetOrderList(ctx, paperAccID, mo.TrdEnvSimulate)
	require.NoError(t, err)
	var sawFilled bool
	for _, o := range ol {
		if o.ClientOrderID == "AUTO-1" && o.IsFullFill() {
			sawFilled = true
		}
	}
	require.True(t, sawFilled, "auto-filled order is retained in Trd_GetOrderList history")

	fl, err := c.GetOrderFillList(ctx, paperAccID, mo.TrdEnvSimulate)
	require.NoError(t, err)
	require.Len(t, fl, 1)
	require.EqualValues(t, 10, fl[0].Qty)
	require.Equal(t, res.VenueOrderID, fl[0].VenueOrderID)
}

func TestVenuePlaceOrderIdempotent(t *testing.T) {
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	bars := fixtureBars(t, "AAPL", start, 3)
	src := mock.NewMemBarSource()
	src.Add("AAPL", qotcommon.KLType_KLType_Day, bars)
	srv := startTradingMock(t, src, mock.VenueConfig{PaperAccID: paperAccID, StartingPower: 1_000_000})
	pc := &pushCollector{}
	c := connectTradingClient(t, srv.Addr(), pc)
	ctx := context.Background()

	req := mo.PlaceOrderRequest{
		AccID: paperAccID, TrdEnv: mo.TrdEnvSimulate, ClientOrderID: "DEDUPE-1",
		Symbol: "AAPL", Side: domain.OrderSideBuy, Type: domain.OrderTypeMarket, TIF: domain.TIFGTC, Qty: 10,
	}
	r1, err := c.PlaceOrder(ctx, req)
	require.NoError(t, err)
	// A SECOND submit with the SAME client order id must NOT create a second
	// venue order — it returns the SAME venue id without re-sending.
	r2, err := c.PlaceOrder(ctx, req)
	require.NoError(t, err)
	require.Equal(t, r1.VenueOrderID, r2.VenueOrderID)

	// Only one working order exists at the venue.
	orders, err := c.GetOrderList(ctx, paperAccID, mo.TrdEnvSimulate)
	require.NoError(t, err)
	count := 0
	for _, o := range orders {
		if o.ClientOrderID == "DEDUPE-1" {
			count++
		}
	}
	require.Equal(t, 1, count, "idempotent re-submit must not create a second venue order")
}

func TestVenueRejects(t *testing.T) {
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	bars := fixtureBars(t, "AAPL", start, 3) // close ~ 100..102
	src := mock.NewMemBarSource()
	src.Add("AAPL", qotcommon.KLType_KLType_Day, bars)
	srv := startTradingMock(t, src, mock.VenueConfig{PaperAccID: paperAccID, StartingPower: 1_000})
	pc := &pushCollector{}
	c := connectTradingClient(t, srv.Addr(), pc)
	ctx := context.Background()

	// warm-up round trip to confirm the connection (diagnostic).
	_, ferr := c.GetFunds(ctx, paperAccID, mo.TrdEnvSimulate)
	t.Logf("warmup GetFunds err=%v", ferr)

	// 1) Unknown symbol: not in the bar source.
	_, err := c.PlaceOrder(ctx, mo.PlaceOrderRequest{
		AccID: paperAccID, TrdEnv: mo.TrdEnvSimulate, ClientOrderID: "R-UNK",
		Symbol: "NOPE", Side: domain.OrderSideBuy, Type: domain.OrderTypeMarket, TIF: domain.TIFGTC, Qty: 1,
	})
	require.Error(t, err, "unknown symbol must be rejected")

	// 2) Insufficient buying power: 100 * ~101 >> 1000 power.
	_, err = c.PlaceOrder(ctx, mo.PlaceOrderRequest{
		AccID: paperAccID, TrdEnv: mo.TrdEnvSimulate, ClientOrderID: "R-BP",
		Symbol: "AAPL", Side: domain.OrderSideBuy, Type: domain.OrderTypeMarket, TIF: domain.TIFGTC, Qty: 100,
	})
	require.Error(t, err, "insufficient buying power must be rejected")

	// 3) Market closed.
	srv.SetMarketClosed(true)
	_, err = c.PlaceOrder(ctx, mo.PlaceOrderRequest{
		AccID: paperAccID, TrdEnv: mo.TrdEnvSimulate, ClientOrderID: "R-CLOSED",
		Symbol: "AAPL", Side: domain.OrderSideBuy, Type: domain.OrderTypeMarket, TIF: domain.TIFGTC, Qty: 1,
	})
	require.Error(t, err, "market closed must be rejected")

	// No position was created by any rejected order.
	require.Empty(t, srv.VenuePositions(paperAccID))

	// Each reject also pushed a terminal SubmitFailed Trd_UpdateOrder.
	requireEventually(t, func() bool {
		orders, _ := pc.snapshot()
		n := 0
		for _, o := range orders {
			if o.Class() == mo.StatusClassRejected {
				n++
			}
		}
		return n >= 3
	}, "expected 3 REJECTED pushes")
}

// requireEventually polls cond until it returns true or a deadline elapses.
func requireEventually(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition not met within deadline: %s", msg)
}
