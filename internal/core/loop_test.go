package core

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/domain"
)

func TestSimClockMonotonic(t *testing.T) {
	c := NewSimClock()
	assert.True(t, c.Now().IsZero())
	t1 := time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)
	require.NoError(t, c.Advance(t1))
	assert.Equal(t, t1, c.Now())
	// Same instant is allowed.
	require.NoError(t, c.Advance(t1))
	// Backwards is rejected.
	err := c.Advance(t1.Add(-time.Hour))
	require.ErrorIs(t, err, ErrTimeReversal)
}

func TestSimClockRejectsNonUTC(t *testing.T) {
	c := NewSimClock()
	loc, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	err = c.Advance(time.Date(2025, 1, 2, 0, 0, 0, 0, loc))
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
}

// TestLoopDispatchOrder confirms the loop drains in total order and advances
// the clock to each event.
func TestLoopDispatchOrder(t *testing.T) {
	loop := NewLoop()
	var seen []string
	rec := func(label string) HandlerFunc {
		return func(_ context.Context, ev Event) error {
			seen = append(seen, label+"@"+ev.TsEvent().Format("01-02"))
			assert.Equal(t, ev.TsEvent(), loop.Now(), "clock advanced to event ts")
			return nil
		}
	}
	loop.Register(KindBar, rec("bar"))
	loop.Register(KindFill, rec("fill"))
	loop.Register(KindSample, rec("sample"))

	t1 := time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)
	t2 := time.Date(2025, 1, 3, 0, 0, 0, 0, time.UTC)
	_, _ = loop.Schedule(SampleEvent{TS: t1})
	_, _ = loop.Schedule(barAt("A", t2))
	_, _ = loop.Schedule(barAt("A", t1))
	_, _ = loop.Schedule(fillAt(t1))

	require.NoError(t, loop.Run(context.Background()))
	assert.Equal(t, []string{"bar@01-02", "fill@01-02", "sample@01-02", "bar@01-03"}, seen)
}

// TestLoopHandlerCanSchedule confirms a handler may enqueue same-ts follow-ons
// (the bar->fill pattern) and they dispatch within the run.
func TestLoopHandlerCanSchedule(t *testing.T) {
	loop := NewLoop()
	t1 := time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)
	var fills int
	loop.Register(KindBar, HandlerFunc(func(_ context.Context, ev Event) error {
		// schedule a fill at the same ts
		_, err := loop.Schedule(fillAt(ev.TsEvent()))
		return err
	}))
	loop.Register(KindFill, HandlerFunc(func(_ context.Context, _ Event) error {
		fills++
		return nil
	}))
	_, _ = loop.Schedule(barAt("A", t1))
	require.NoError(t, loop.Run(context.Background()))
	assert.Equal(t, 1, fills)
}

// TestLoopContextCancellation stops cleanly at an event boundary.
func TestLoopContextCancellation(t *testing.T) {
	loop := NewLoop()
	ctx, cancel := context.WithCancel(context.Background())
	t1 := time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)
	var handled int
	loop.Register(KindBar, HandlerFunc(func(_ context.Context, _ Event) error {
		handled++
		if handled == 2 {
			cancel() // cancel mid-run; loop stops before the next event
		}
		return nil
	}))
	for i := 0; i < 10; i++ {
		_, err := loop.Schedule(barAt("A", t1.AddDate(0, 0, i)))
		require.NoError(t, err)
	}
	err := loop.Run(ctx)
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 2, handled, "loop stopped at the event boundary after cancel")
}

// TestLoopRejectsBackwardsSchedule rejects events before the clock.
func TestLoopRejectsBackwardsSchedule(t *testing.T) {
	loop := NewLoop()
	t1 := time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)
	loop.Register(KindBar, HandlerFunc(func(_ context.Context, _ Event) error {
		// try to schedule an event before the current clock
		_, err := loop.Schedule(barAt("A", t1.Add(-time.Hour)))
		return err
	}))
	_, _ = loop.Schedule(barAt("A", t1))
	err := loop.Run(context.Background())
	require.ErrorIs(t, err, ErrTimeReversal)
}

// TestLoopClosedAfterRun rejects scheduling after the loop returns.
func TestLoopClosedAfterRun(t *testing.T) {
	loop := NewLoop()
	loop.Register(KindBar, HandlerFunc(func(_ context.Context, _ Event) error { return nil }))
	require.NoError(t, loop.Run(context.Background()))
	_, err := loop.Schedule(barAt("A", time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)))
	require.ErrorIs(t, err, ErrLoopClosed)
}

// TestLoopHandlerErrorAborts surfaces a handler error.
func TestLoopHandlerErrorAborts(t *testing.T) {
	loop := NewLoop()
	sentinel := errors.New("boom")
	loop.Register(KindBar, HandlerFunc(func(_ context.Context, _ Event) error { return sentinel }))
	_, _ = loop.Schedule(barAt("A", time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)))
	err := loop.Run(context.Background())
	require.ErrorIs(t, err, sentinel)
}

// TestMsgBusOrder confirms observers fire in registration order, messages in
// publish order.
func TestMsgBusOrder(t *testing.T) {
	bus := NewMsgBus()
	var log []string
	bus.SubscribeFills(fillRec{"o1", &log})
	bus.SubscribeFills(fillRec{"o2", &log})
	bus.PublishFill(domain.Fill{TradeID: "t1"})
	bus.PublishFill(domain.Fill{TradeID: "t2"})
	assert.Equal(t, []string{"o1:t1", "o2:t1", "o1:t2", "o2:t2"}, log)
}

type fillRec struct {
	name string
	log  *[]string
}

func (r fillRec) OnFill(f domain.Fill) { *r.log = append(*r.log, r.name+":"+f.TradeID) }
