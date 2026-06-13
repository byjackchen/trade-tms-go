package runner_test

// feed_test.go is the mock-driven live-feed integration test: it drives a real
// moomoo.Client (native OpenD wire protocol) against the in-repo protocol-
// faithful mock OpenD, bridges the Qot_UpdateKL pushes through the MoomooFeed
// into a livengine.Session, and proves intents are emitted — the gate driver
// for the live path that needs NO Postgres and NO real OpenD (decision 2/7).

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/adapters/moomoo"
	"github.com/byjackchen/trade-tms-go/internal/adapters/moomoo/mock"
	"github.com/byjackchen/trade-tms-go/internal/adapters/moomoo/pb/qotcommon"
	"github.com/byjackchen/trade-tms-go/internal/core"
	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/engine/strategyassembly"
	"github.com/byjackchen/trade-tms-go/internal/livengine"
	"github.com/byjackchen/trade-tms-go/internal/publish"
	"github.com/byjackchen/trade-tms-go/internal/runner"
)

// TestLiveFeedOverMockOpenD drives the full live feed path over the mock OpenD:
// real client wire frames -> Qot_Sub -> Qot_UpdateKL push -> MoomooFeed ->
// livengine.Session -> intents. It proves the streaming live wiring end to end
// without Postgres or real OpenD.
func TestLiveFeedOverMockOpenD(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// (1) Mock OpenD over a MemBarSource (no Postgres). It needs a source even
	// though this test only exercises the push path.
	src := mock.NewMemBarSource()
	srv, err := mock.New(mock.Options{Source: src})
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Close() })
	go func() { _ = srv.Serve(ctx) }()

	// (2) The 8-ETF wide sector universe (matches paramsSector()).
	syms := []string{"E1", "E2", "E3", "E4", "E5", "E6", "E7", "E8"}
	kl := qotcommon.KLType_KLType_Day

	feed := runner.NewMoomooFeed(syms, kl, 0, zerolog.Nop())
	client := moomoo.NewClient(moomoo.Options{
		Addr:    srv.Addr(),
		Logger:  zerolog.Nop(),
		OnKLine: feed.PushHandler,
	})
	client.Start(ctx)
	t.Cleanup(func() { _ = client.Close() })
	require.NoError(t, client.Ready(ctx))
	require.NoError(t, feed.Subscribe(ctx, client))

	// (3) A signal-mode sector session writing into a MemSink, over the live feed
	// (wall clock — the feed produces bars as the mock pushes them).
	asm, err := strategyassembly.Assemble(strategyassembly.Input{
		Strategy:        "sector_rotation",
		StartingBalance: 100000,
		Params:          strategyassembly.Params{Sector: paramsSector()},
	})
	require.NoError(t, err)
	sink := livengine.NewMemSink()
	sess, err := livengine.NewSession(livengine.Config{
		Mode:            livengine.ModeSignal,
		Strategies:      asm.Strategies,
		Portfolio:       asm.Portfolio,
		StartingBalance: domain.MustMoney("100000"),
		Sink:            sink,
	})
	require.NoError(t, err)
	require.NoError(t, sess.Prime(ctx))

	// (4) Run the session in the background; push a rising day of bars through the
	// mock, then stop the session and assert intents were emitted.
	runCtx, cancelRun := context.WithCancel(ctx)
	runErr := make(chan error, 1)
	go func() { runErr <- sess.RunStream(runCtx, feed, core.StreamWall, nil) }()

	dates := []time.Time{
		time.Date(2024, time.January, 2, 0, 0, 0, 0, time.UTC),
		time.Date(2024, time.January, 16, 0, 0, 0, 0, time.UTC),
		time.Date(2024, time.January, 31, 0, 0, 0, 0, time.UTC),
		time.Date(2024, time.February, 1, 0, 0, 0, 0, time.UTC),
	}
	for i, d := range dates {
		px := domain.MustPrice(intToString(100+i) + ".00")
		for _, s := range syms {
			bar := domain.Bar{Symbol: s, TS: d, Open: px, High: px, Low: px, Close: px, Volume: 1000}
			// Retry the push until at least one connection is registered (the
			// client's Qot_Sub registers the push asynchronously after Subscribe).
			require.Eventually(t, func() bool {
				n, perr := srv.PushKLine(s, kl, []domain.Bar{bar})
				return perr == nil && n >= 1
			}, 5*time.Second, 20*time.Millisecond, "push %s@%s should reach the client", s, d)
		}
	}

	// The session emits ONE intent record per strategy per flushed timestamp (the
	// sector adapter returns the whole per-ETF slice as one payload; the per-ETF
	// fan-out happens later in the publish sink). With one strategy, the first 3
	// timestamps flush on rollover (the 4th flushes on stream close).
	require.Eventually(t, func() bool {
		return sess.EmittedIntents() >= 3 && sess.BarsSeen() >= 32
	}, 10*time.Second, 100*time.Millisecond, "live feed should drive intent emission")
	assert.Equal(t, 32, sess.BarsSeen(), "all 4 timestamps x 8 ETFs delivered over the mock feed")

	cancelRun()
	<-runErr // RunStream returns (ctx canceled flushes the final timestamp)

	assert.Positive(t, sess.EmittedIntents(), "intents emitted over the live mock feed")
	require.NotEmpty(t, sink.Intents, "MemSink recorded intents")

	// Each recorded payload is a per-ETF SectorRotationIntent slice; normalize it
	// through the publish layer to prove the full live wire shape resolves.
	total := 0
	for _, r := range sink.Intents {
		norms, nerr := publish.NormalizeIntent(r.Payload)
		require.NoError(t, nerr)
		total += len(norms)
	}
	assert.Positive(t, total, "normalized per-ETF intents from the live feed")
}
