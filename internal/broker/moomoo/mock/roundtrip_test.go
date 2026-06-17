package mock_test

// roundtrip_test.go drives the native Go client against the mock OpenD server
// over a real TCP socket, exercising every P5 message type end to end:
// handshake, GetGlobalState, KeepAlive, Subscribe + Qot_UpdateKL push,
// RequestHistoryKL, GetKL, GetBasicQot, GetSubInfo, and the reconnect +
// re-subscribe path. The mock serves an in-memory bar fixture so the gate runs
// without Postgres; a separate integration test (pg_source_test.go, tagged)
// exercises the Postgres-backed source.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	mo "github.com/byjackchen/trade-tms-go/internal/broker/moomoo"
	"github.com/byjackchen/trade-tms-go/internal/broker/moomoo/mock"
	"github.com/byjackchen/trade-tms-go/internal/broker/moomoo/pb/qotcommon"
	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// fixtureBars builds n ascending daily bars for symbol starting at start.
func fixtureBars(t *testing.T, symbol string, start time.Time, n int) []domain.Bar {
	t.Helper()
	bars := make([]domain.Bar, n)
	for i := 0; i < n; i++ {
		ts := start.AddDate(0, 0, i).UTC()
		base := 100.0 + float64(i)
		o := domain.MustPrice(price(base))
		h := domain.MustPrice(price(base + 2))
		l := domain.MustPrice(price(base - 1))
		c := domain.MustPrice(price(base + 1))
		bars[i] = domain.Bar{Symbol: symbol, TS: ts, Open: o, High: h, Low: l, Close: c, Volume: int64(1000 + i)}
	}
	return bars
}

func price(f float64) string {
	p, err := domain.PriceFromFloat64(f)
	if err != nil {
		panic(err)
	}
	return p.String()
}

// startMock spins up a mock server serving src and returns it with a cleanup.
func startMock(t *testing.T, src mock.BarSource) (*mock.Server, context.CancelFunc) {
	t.Helper()
	srv, err := mock.New(mock.Options{
		Listen:            "127.0.0.1:0",
		Source:            src,
		KeepAliveInterval: 1,
		Now:               func() time.Time { return time.Date(2024, 6, 13, 14, 0, 0, 0, time.UTC) },
	})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		_ = srv.Close()
	})
	return srv, cancel
}

func TestClientMockRoundTrip(t *testing.T) {
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	aapl := fixtureBars(t, "AAPL", start, 30)
	msft := fixtureBars(t, "MSFT", start, 30)
	src := mock.NewMemBarSource()
	src.Add("AAPL", qotcommon.KLType_KLType_Day, aapl)
	src.Add("MSFT", qotcommon.KLType_KLType_Day, msft)

	srv, _ := startMock(t, src)

	var (
		mu       sync.Mutex
		gotPush  []domain.Bar
		pushSyms []string
	)
	client := mo.NewClient(mo.Options{
		Addr: srv.Addr(),
		OnKLine: func(symbol string, kl qotcommon.KLType, bars []domain.Bar) {
			mu.Lock()
			defer mu.Unlock()
			pushSyms = append(pushSyms, symbol)
			gotPush = append(gotPush, bars...)
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client.Start(ctx)
	defer client.Close()
	require.NoError(t, client.Ready(ctx))

	// GetGlobalState.
	gs, err := client.GetGlobalState(ctx)
	require.NoError(t, err)
	require.True(t, gs.QotLogined)
	require.Equal(t, qotcommon.QotMarketState_QotMarketState_Morning, gs.MarketUS)

	// RequestHistoryKL over the whole window.
	hist, err := client.RequestHistoryKL(ctx, "AAPL", qotcommon.KLType_KLType_Day, start, start.AddDate(0, 0, 40))
	require.NoError(t, err)
	require.Len(t, hist, 30)
	require.Equal(t, aapl[0].Close, hist[0].Close)
	require.Equal(t, aapl[29].TS.Unix(), hist[29].TS.Unix())

	// Subscribe (registers push) then push a "live" bar and observe it.
	require.NoError(t, client.Subscribe(ctx, []string{"AAPL", "MSFT"}, qotcommon.KLType_KLType_Day))
	require.Len(t, client.Subscriptions(), 2)

	// GetKL (served from the same source) — most-recent 5.
	kl, err := client.GetKL(ctx, "AAPL", qotcommon.KLType_KLType_Day, 5)
	require.NoError(t, err)
	require.Len(t, kl, 5)
	require.Equal(t, aapl[29].Close, kl[4].Close)

	// GetBasicQot.
	quotes, err := client.GetBasicQot(ctx, []string{"AAPL"})
	require.NoError(t, err)
	require.Len(t, quotes, 1)
	require.Equal(t, "AAPL", quotes[0].Symbol)
	require.Equal(t, aapl[29].Close, quotes[0].CurPrice)

	// GetSubInfo reflects 2 used.
	si, err := client.GetSubInfo(ctx)
	require.NoError(t, err)
	require.Equal(t, int32(2), si.TotalUsedQuota)

	// Push a new bar for AAPL; the handler must receive it.
	liveBar := fixtureBars(t, "AAPL", start.AddDate(0, 0, 30), 1)
	require.Eventually(t, func() bool {
		n, err := srv.PushKLine("AAPL", qotcommon.KLType_KLType_Day, liveBar)
		return err == nil && n == 1
	}, 3*time.Second, 50*time.Millisecond, "push should reach the registered connection")

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(gotPush) >= 1
	}, 3*time.Second, 20*time.Millisecond, "client should receive the push")

	mu.Lock()
	require.Equal(t, "AAPL", pushSyms[0])
	require.Equal(t, liveBar[0].Close, gotPush[0].Close)
	mu.Unlock()
}

// TestClientReconnectResubscribe verifies that after the server drops the
// connection (WITHOUT stopping the listener), the client reconnects with
// backoff AND replays its subscription set on the fresh connection — so pushes
// resume without the caller re-subscribing. This is the production reconnect
// guarantee, exercised against a single stable mock address.
func TestClientReconnectResubscribe(t *testing.T) {
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	src := mock.NewMemBarSource()
	src.Add("AAPL", qotcommon.KLType_KLType_Day, fixtureBars(t, "AAPL", start, 10))
	srv, _ := startMock(t, src)

	var mu sync.Mutex
	pushes := 0
	client := mo.NewClient(mo.Options{
		Addr:       srv.Addr(),
		MinBackoff: 20 * time.Millisecond,
		MaxBackoff: 100 * time.Millisecond,
		OnKLine: func(string, qotcommon.KLType, []domain.Bar) {
			mu.Lock()
			pushes++
			mu.Unlock()
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client.Start(ctx)
	defer client.Close()
	require.NoError(t, client.Ready(ctx))
	require.NoError(t, client.Subscribe(ctx, []string{"AAPL"}, qotcommon.KLType_KLType_Day))
	require.Eventually(t, func() bool { return srv.Conns() >= 1 }, 2*time.Second, 20*time.Millisecond)

	// Sanity: a push reaches us BEFORE the drop.
	pushBar := fixtureBars(t, "AAPL", start.AddDate(0, 0, 20), 1)
	require.Eventually(t, func() bool {
		n, _ := srv.PushKLine("AAPL", qotcommon.KLType_KLType_Day, pushBar)
		return n == 1
	}, 2*time.Second, 50*time.Millisecond)
	require.Eventually(t, func() bool { mu.Lock(); defer mu.Unlock(); return pushes >= 1 }, 3*time.Second, 20*time.Millisecond)

	// Force a transient drop; the listener stays up.
	dropped := srv.DropConns()
	require.GreaterOrEqual(t, dropped, 1)

	// The client must reconnect AND replay its subscription. We confirm by
	// waiting for a fresh server-side connection whose subscription registry
	// has been re-populated — i.e. a post-drop push reaches the client.
	require.Eventually(t, func() bool { return srv.Conns() >= 1 }, 5*time.Second, 20*time.Millisecond,
		"client should re-establish a connection")

	// Give re-subscribe a moment, then a post-reconnect push must land.
	var afterDrop int
	require.Eventually(t, func() bool {
		mu.Lock()
		before := pushes
		mu.Unlock()
		n, _ := srv.PushKLine("AAPL", qotcommon.KLType_KLType_Day, pushBar)
		if n < 1 {
			return false // re-subscribe not replayed yet
		}
		time.Sleep(50 * time.Millisecond)
		mu.Lock()
		afterDrop = pushes - before
		mu.Unlock()
		return afterDrop >= 1
	}, 6*time.Second, 100*time.Millisecond, "subscription must be replayed after reconnect")
}
