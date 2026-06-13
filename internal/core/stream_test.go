package core

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// feedBars builds a StreamSource that delivers the given bars then closes,
// honouring ctx cancellation (no leak).
func feedBars(ctx context.Context, bars []BarEvent, buf int) StreamSource {
	ch := make(chan StreamEvent, buf)
	go func() {
		defer close(ch)
		for _, b := range bars {
			select {
			case <-ctx.Done():
				return
			case ch <- StreamEvent{Event: b}:
			}
		}
	}()
	return NewChannelSource(ch)
}

// TestStreamLoopVirtualFollowsEventTime confirms the virtual discipline sets the
// clock to each event's timestamp before dispatch, in delivery order.
func TestStreamLoopVirtualFollowsEventTime(t *testing.T) {
	vc := NewVirtualClock(time.Time{})
	loop := NewStreamLoop(StreamVirtual, vc)

	t1 := time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)
	t2 := time.Date(2025, 1, 3, 0, 0, 0, 0, time.UTC)
	var seen []string
	loop.Register(KindBar, HandlerFunc(func(_ context.Context, ev Event) error {
		assert.Equal(t, ev.TsEvent(), loop.Now(), "virtual clock at event ts")
		seen = append(seen, ev.(BarEvent).Bar.Symbol+"@"+ev.TsEvent().Format("01-02"))
		return nil
	}))

	src := feedBars(context.Background(), []BarEvent{
		barAt("A", t1), barAt("B", t1), barAt("A", t2),
	}, 0)
	require.NoError(t, loop.RunStream(context.Background(), src))
	assert.Equal(t, []string{"A@01-02", "B@01-02", "A@01-03"}, seen)
}

// TestStreamLoopWallDoesNotRewind confirms the wall discipline reads real time
// and never goes backwards across dispatches.
func TestStreamLoopWallDoesNotRewind(t *testing.T) {
	loop := NewStreamLoop(StreamWall, nil)
	t1 := time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)
	var prev time.Time
	loop.Register(KindBar, HandlerFunc(func(_ context.Context, _ Event) error {
		now := loop.Now()
		assert.False(t, now.Before(prev), "wall clock monotonic non-decreasing")
		_, off := now.Zone()
		assert.Equal(t, 0, off, "wall clock is UTC")
		prev = now
		return nil
	}))
	bars := make([]BarEvent, 5)
	for i := range bars {
		bars[i] = barAt("A", t1.AddDate(0, 0, i))
	}
	require.NoError(t, loop.RunStream(context.Background(), feedBars(context.Background(), bars, 5)))
}

// TestStreamLoopHandlerSchedulesFollowOn confirms a handler-scheduled same-ts
// event drains before the next source event (intra-timestamp ordering).
func TestStreamLoopHandlerSchedulesFollowOn(t *testing.T) {
	vc := NewVirtualClock(time.Time{})
	loop := NewStreamLoop(StreamVirtual, vc)
	t1 := time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)
	var order []string
	loop.Register(KindBar, HandlerFunc(func(_ context.Context, ev Event) error {
		order = append(order, "bar")
		_, err := loop.Schedule(fillAt(ev.TsEvent()))
		return err
	}))
	loop.Register(KindFill, HandlerFunc(func(_ context.Context, _ Event) error {
		order = append(order, "fill")
		return nil
	}))
	src := feedBars(context.Background(), []BarEvent{barAt("A", t1), barAt("A", t1.AddDate(0, 0, 1))}, 0)
	require.NoError(t, loop.RunStream(context.Background(), src))
	// each bar's fill drains before the next bar.
	assert.Equal(t, []string{"bar", "fill", "bar", "fill"}, order)
}

// TestStreamLoopRejectsBackwardsDelivery aborts on a non-decreasing-ts violation.
func TestStreamLoopRejectsBackwardsDelivery(t *testing.T) {
	vc := NewVirtualClock(time.Time{})
	loop := NewStreamLoop(StreamVirtual, vc)
	loop.Register(KindBar, HandlerFunc(func(_ context.Context, _ Event) error { return nil }))
	t1 := time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)
	src := feedBars(context.Background(), []BarEvent{barAt("A", t1), barAt("A", t1.Add(-time.Hour))}, 0)
	err := loop.RunStream(context.Background(), src)
	require.ErrorIs(t, err, ErrTimeReversal)
}

// TestStreamLoopContextCancellation stops cleanly without draining the source.
func TestStreamLoopContextCancellation(t *testing.T) {
	vc := NewVirtualClock(time.Time{})
	loop := NewStreamLoop(StreamVirtual, vc)
	ctx, cancel := context.WithCancel(context.Background())
	t1 := time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)
	var handled int
	loop.Register(KindBar, HandlerFunc(func(_ context.Context, _ Event) error {
		handled++
		if handled == 2 {
			cancel()
		}
		return nil
	}))
	bars := make([]BarEvent, 100)
	for i := range bars {
		bars[i] = barAt("A", t1.AddDate(0, 0, i))
	}
	// buffered so the producer is not blocked when we cancel.
	src := feedBars(ctx, bars, 100)
	err := loop.RunStream(ctx, src)
	require.ErrorIs(t, err, context.Canceled)
	assert.LessOrEqual(t, handled, 3, "loop stopped near the cancel boundary")
}

// TestVirtualClockSetUTC confirms Set stores UTC and Now reports it.
func TestVirtualClockSetUTC(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	vc := NewVirtualClock(time.Time{})
	vc.Set(time.Date(2025, 1, 2, 9, 30, 0, 0, loc))
	_, off := vc.Now().Zone()
	assert.Equal(t, 0, off, "VirtualClock normalizes to UTC")
}
