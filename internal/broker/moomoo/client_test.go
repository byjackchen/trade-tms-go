package moomoo

import (
	"context"
	"math/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/broker/moomoo/pb/qotcommon"
)

func TestNextSerialNeverZeroAndMonotone(t *testing.T) {
	c := NewClient(Options{Addr: "127.0.0.1:1"})
	prev := c.nextSerial()
	for i := 0; i < 1000; i++ {
		s := c.nextSerial()
		require.NotZero(t, s)
		require.Equal(t, prev+1, s)
		prev = s
	}
}

func TestBackoffBounded(t *testing.T) {
	c := NewClient(Options{
		Addr:       "127.0.0.1:1",
		MinBackoff: 100 * time.Millisecond,
		MaxBackoff: 2 * time.Second,
		rng:        rand.New(rand.NewSource(1)),
	})
	for attempt := 1; attempt <= 20; attempt++ {
		b := c.backoffFor(attempt)
		require.GreaterOrEqual(t, b, c.opts.MinBackoff, "attempt %d below min", attempt)
		require.LessOrEqual(t, b, c.opts.MaxBackoff, "attempt %d above max", attempt)
	}
}

func TestRequestsFailWhenNotConnected(t *testing.T) {
	c := NewClient(Options{Addr: "127.0.0.1:1"})
	// Not started: runCtx is nil; guard against that by starting then closing.
	ctx := context.Background()
	c.Start(ctx)
	require.NoError(t, c.Close())

	_, err := c.GetGlobalState(context.Background())
	require.ErrorIs(t, err, ErrClosed)
}

func TestSubscriptionCapEnforced(t *testing.T) {
	c := NewClient(Options{Addr: "127.0.0.1:1", MaxSubscriptions: 3})
	// reserveQuota is the cap gate used by Subscribe before any I/O.
	require.NoError(t, c.reserveQuota([]string{"A", "B"}, qotcommon.KLType_KLType_Day))
	require.NoError(t, c.reserveQuota([]string{"B", "C"}, qotcommon.KLType_KLType_Day)) // B dup, +C => 3
	require.Len(t, c.Subscriptions(), 3)

	err := c.reserveQuota([]string{"D"}, qotcommon.KLType_KLType_Day)
	require.Error(t, err, "4th distinct subscription must exceed cap 3")

	c.releaseQuota([]string{"A"}, qotcommon.KLType_KLType_Day)
	require.Len(t, c.Subscriptions(), 2)
	require.NoError(t, c.reserveQuota([]string{"D"}, qotcommon.KLType_KLType_Day))
}

func TestCloseIdempotentAndNoLeak(t *testing.T) {
	c := NewClient(Options{Addr: "127.0.0.1:1", MinBackoff: time.Millisecond, MaxBackoff: 5 * time.Millisecond})
	ctx := context.Background()
	c.Start(ctx)
	// Let the supervisor spin a few failed-dial/backoff cycles.
	time.Sleep(30 * time.Millisecond)
	require.NoError(t, c.Close())
	require.NoError(t, c.Close()) // idempotent
	// Ready after close returns promptly with ErrClosed.
	require.ErrorIs(t, c.Ready(context.Background()), ErrClosed)
}
