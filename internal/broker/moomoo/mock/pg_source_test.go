//go:build integration

package mock_test

// pg_source_test.go drives the mock OpenD server's Postgres-backed BarSource
// end to end against the compose stack: it inserts a handful of bars into
// tms.bars_daily / tms.bars_intraday, serves them through the mock to the
// native client via Qot_RequestHistoryKL, and asserts the round-tripped bars
// match what was stored (exact fixed-point prices). Run with `make itest` (sets
// TMS_PG_HOST/PORT); skipped otherwise.

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	mo "github.com/byjackchen/trade-tms-go/internal/broker/moomoo"
	"github.com/byjackchen/trade-tms-go/internal/broker/moomoo/mock"
	"github.com/byjackchen/trade-tms-go/internal/broker/moomoo/pb/qotcommon"
	"github.com/byjackchen/trade-tms-go/internal/config"
	"github.com/byjackchen/trade-tms-go/internal/db"
	"github.com/byjackchen/trade-tms-go/internal/domain"
)

func itestPool(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("TMS_PG_HOST") == "" || os.Getenv("TMS_PG_PORT") == "" {
		t.Skip("integration: TMS_PG_HOST/TMS_PG_PORT not set (use `make itest`)")
	}
	cfg, err := config.Load()
	require.NoError(t, err)
	pool, err := db.NewPool(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

const pgTestTicker = "ZZMOCKTEST"

func TestPGBarSourceThroughMock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := itestPool(t, ctx)

	// Insert a small daily series; clean up after.
	start := time.Date(2024, 3, 1, 5, 0, 0, 0, time.UTC) // NY-midnight-ish
	want := make([]domain.Bar, 5)
	for i := range want {
		ts := start.AddDate(0, 0, i)
		o := domain.MustPrice("100.10")
		h := domain.MustPrice("102.20")
		l := domain.MustPrice("99.30")
		c := domain.MustPrice("101.40")
		want[i] = domain.Bar{Symbol: pgTestTicker, TS: ts, Open: o, High: h, Low: l, Close: c, Volume: int64(500 + i)}
		_, err := pool.Exec(ctx, `
			INSERT INTO tms.bars_daily (ticker, ts, source, open, high, low, close, volume)
			VALUES ($1,$2,'SEP',$3,$4,$5,$6,$7)
			ON CONFLICT (ticker, ts, source) DO UPDATE SET
			  open=EXCLUDED.open, high=EXCLUDED.high, low=EXCLUDED.low,
			  close=EXCLUDED.close, volume=EXCLUDED.volume`,
			pgTestTicker, ts, o.Raw(), h.Raw(), l.Raw(), c.Raw(), int64(500+i))
		require.NoError(t, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM tms.bars_daily WHERE ticker=$1`, pgTestTicker)
	})

	src := mock.NewPGBarSource(pool)
	srv, err := mock.New(mock.Options{Listen: "127.0.0.1:0", Source: src, KeepAliveInterval: 5})
	require.NoError(t, err)
	sctx, scancel := context.WithCancel(ctx)
	defer scancel()
	go func() { _ = srv.Serve(sctx) }()
	defer srv.Close()

	client := mo.NewClient(mo.Options{Addr: srv.Addr()})
	client.Start(ctx)
	defer client.Close()
	require.NoError(t, client.Ready(ctx))

	got, err := client.RequestHistoryKL(ctx, pgTestTicker, qotcommon.KLType_KLType_Day,
		start.AddDate(0, 0, -1), start.AddDate(0, 0, 10))
	require.NoError(t, err)
	require.Len(t, got, len(want))
	for i := range want {
		require.Equal(t, want[i].TS.Unix(), got[i].TS.Unix(), "bar %d ts", i)
		require.Equal(t, want[i].Close, got[i].Close, "bar %d close", i)
		require.Equal(t, want[i].Open, got[i].Open, "bar %d open", i)
		require.Equal(t, want[i].Volume, got[i].Volume, "bar %d volume", i)
	}
}
