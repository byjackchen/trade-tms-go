package scheduler

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/jobs"
	"github.com/byjackchen/trade-tms-go/internal/jobs/handlers"
)

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

// fakeLedger is an in-memory Ledger: it reproduces the PG unique-slot dedupe
// (exactly one Won=true per (pipeline, trading_date)) without a database, so
// the controllable-clock tests stay hermetic. It is safe for concurrent use
// to mirror the real "multiple instances" race the integration test covers.
type fakeLedger struct {
	mu     sync.Mutex
	nextID int64
	slots  map[string]int64 // "pipeline|date" -> run id
	jobs   map[int64][2]int64
	// claimErr, when set, makes Claim fail (fault injection).
	claimErr error
}

func newFakeLedger() *fakeLedger {
	return &fakeLedger{slots: map[string]int64{}, jobs: map[int64][2]int64{}}
}

func (f *fakeLedger) Claim(_ context.Context, pipeline string, d calendar.Date, _ string, _ Trigger) (ClaimResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claimErr != nil {
		return ClaimResult{}, f.claimErr
	}
	key := pipeline + "|" + d.String()
	if _, ok := f.slots[key]; ok {
		return ClaimResult{Won: false}, nil
	}
	f.nextID++
	f.slots[key] = f.nextID
	return ClaimResult{Won: true, RunID: f.nextID}, nil
}

func (f *fakeLedger) RecordJobs(_ context.Context, runID int64, dataJobID, eodJobID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobs[runID] = [2]int64{dataJobID, eodJobID}
	return nil
}

func (f *fakeLedger) claimCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.slots)
}

// recordedEnqueue captures one Enqueue call.
type recordedEnqueue struct {
	Kind      string
	DedupeKey string
	Payload   any
	RunAt     time.Time
	Priority  int32
}

// fakeQueue records every Enqueue and hands back monotonic job ids. It honors
// DedupeKey so a repeated key returns the existing job with deduped=true, like
// the real active-job dedupe index.
type fakeQueue struct {
	mu     sync.Mutex
	nextID int64
	calls  []recordedEnqueue
	byKey  map[string]*jobs.Job
}

func newFakeQueue() *fakeQueue { return &fakeQueue{byKey: map[string]*jobs.Job{}} }

func (q *fakeQueue) Enqueue(_ context.Context, p jobs.EnqueueParams) (*jobs.Job, bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.calls = append(q.calls, recordedEnqueue{
		Kind: p.Kind, DedupeKey: p.DedupeKey, Payload: p.Payload, RunAt: p.RunAt, Priority: p.Priority,
	})
	if p.DedupeKey != "" {
		if j, ok := q.byKey[p.DedupeKey]; ok {
			return j, true, nil
		}
	}
	q.nextID++
	j := &jobs.Job{ID: q.nextID, Kind: p.Kind, Status: jobs.StatusQueued}
	if p.DedupeKey != "" {
		q.byKey[p.DedupeKey] = j
	}
	return j, false, nil
}

func (q *fakeQueue) kinds() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]string, len(q.calls))
	for i, c := range q.calls {
		out[i] = c.Kind
	}
	return out
}

func (q *fakeQueue) callsFor(kind string) []recordedEnqueue {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []recordedEnqueue
	for _, c := range q.calls {
		if c.Kind == kind {
			out = append(out, c)
		}
	}
	return out
}

// clock is a controllable monotonic-ish clock the tests advance explicitly.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock(t time.Time) *clock { return &clock{t: t} }
func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *clock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func nyLoc(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	return loc
}

func newTestScheduler(t *testing.T, clk *clock, opts Options) (*Scheduler, *fakeQueue, *fakeLedger) {
	t.Helper()
	cal, err := calendar.NewNYSE()
	require.NoError(t, err)
	q := newFakeQueue()
	l := newFakeLedger()
	if opts.Now == nil {
		opts.Now = clk.now
	}
	s, err := New(cal, q, l, zerolog.Nop(), opts)
	require.NoError(t, err)
	return s, q, l
}

// at builds a New-York wall-clock instant.
func at(t *testing.T, y int, mo time.Month, d, h, mi int) time.Time {
	t.Helper()
	return time.Date(y, mo, d, h, mi, 0, 0, nyLoc(t))
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestParseTimeOfDay(t *testing.T) {
	tod, err := ParseTimeOfDay("18:30")
	require.NoError(t, err)
	assert.Equal(t, TimeOfDay{Hour: 18, Minute: 30}, tod)
	assert.Equal(t, "18:30", tod.String())

	for _, bad := range []string{"", "1830", "25:00", "18:60", "6:30pm", "18-30"} {
		_, err := ParseTimeOfDay(bad)
		assert.Error(t, err, "expected %q to be rejected", bad)
	}
}

// 2026-06-15 is a Monday (a normal NYSE trading day) — the anchor for most
// cases. 18:30 ET is the configured fire time.
func dailyAt1830(t *testing.T) TimeOfDay { return TimeOfDay{Hour: 18, Minute: 30} }

func TestTickEnqueuesExactlyOncePerTradingDay(t *testing.T) {
	// Before fire time: waiting; after: one enqueue; subsequent ticks: no-op.
	clk := newClock(at(t, 2026, time.June, 15, 9, 0)) // Mon 09:00 ET
	s, q, l := newTestScheduler(t, clk, Options{DailyAt: dailyAt1830(t), Loc: nyLoc(t)})
	ctx := context.Background()

	// 09:00 — before fire: waiting, nothing enqueued.
	res, err := s.Tick(ctx)
	require.NoError(t, err)
	assert.Equal(t, ActionWaiting, res.Action)
	assert.Empty(t, q.kinds())

	// 18:30 — fire: enqueue the pipeline (data.refresh then eod.refresh).
	clk.set(at(t, 2026, time.June, 15, 18, 30))
	res, err = s.Tick(ctx)
	require.NoError(t, err)
	require.Equal(t, ActionEnqueued, res.Action)
	assert.Equal(t, []string{handlers.KindDataRefresh, handlers.KindEODRefresh}, q.kinds())
	assert.Positive(t, res.Pipeline.DataJobID)
	assert.Positive(t, res.Pipeline.EODJobID)

	// data.refresh carries source=api; eod.refresh carries as_of = the
	// trading date and a run_at floored AFTER the data refresh.
	dataCalls := q.callsFor(handlers.KindDataRefresh)
	require.Len(t, dataCalls, 1)
	assert.Equal(t, map[string]any{"source": "api"}, dataCalls[0].Payload)
	assert.Equal(t, dataRefreshPriority, int(dataCalls[0].Priority))
	eodCalls := q.callsFor(handlers.KindEODRefresh)
	require.Len(t, eodCalls, 1)
	assert.Equal(t, "2026-06-15", eodCalls[0].Payload.(map[string]any)["as_of"])
	assert.False(t, eodCalls[0].RunAt.IsZero(), "eod run_at must be floored after data refresh")
	assert.True(t, eodCalls[0].RunAt.After(clk.now()), "eod run_at must be in the future relative to fire time")

	// 18:31, 19:00, next morning same date window — all no-op (slot claimed).
	for _, ts := range []time.Time{
		at(t, 2026, time.June, 15, 18, 31),
		at(t, 2026, time.June, 15, 19, 0),
		at(t, 2026, time.June, 15, 23, 59),
	} {
		clk.set(ts)
		res, err = s.Tick(ctx)
		require.NoError(t, err)
		assert.Equal(t, ActionAlreadyDone, res.Action, "ts=%s", ts)
	}
	// Exactly one data.refresh + one eod.refresh enqueued for the whole day.
	assert.Len(t, q.callsFor(handlers.KindDataRefresh), 1)
	assert.Len(t, q.callsFor(handlers.KindEODRefresh), 1)
	assert.Equal(t, 1, l.claimCount())
}

func TestTickSkipsWeekend(t *testing.T) {
	// 2026-06-13 is a Saturday, 2026-06-14 a Sunday — both non-trading.
	for _, day := range []int{13, 14} {
		clk := newClock(at(t, 2026, time.June, day, 18, 30))
		s, q, _ := newTestScheduler(t, clk, Options{DailyAt: dailyAt1830(t), Loc: nyLoc(t)})
		res, err := s.Tick(context.Background())
		require.NoError(t, err)
		assert.Equal(t, ActionSkippedNonTradingDay, res.Action, "2026-06-%02d", day)
		assert.Empty(t, q.kinds(), "no pipeline on a weekend")
	}
}

func TestTickSkipsHoliday(t *testing.T) {
	// 2026-07-03 is the observed Independence Day (July 4 falls on a Saturday),
	// an NYSE holiday. Even past fire time, nothing is enqueued.
	cal, err := calendar.NewNYSE()
	require.NoError(t, err)
	holiday := calendar.NewDate(2026, time.July, 3)
	trading, err := cal.IsTradingDay(holiday)
	require.NoError(t, err)
	require.False(t, trading, "2026-07-03 must be a holiday for this test's premise")

	clk := newClock(at(t, 2026, time.July, 3, 20, 0))
	s, q, _ := newTestScheduler(t, clk, Options{DailyAt: dailyAt1830(t), Loc: nyLoc(t)})
	res, err := s.Tick(context.Background())
	require.NoError(t, err)
	assert.Equal(t, ActionSkippedNonTradingDay, res.Action)
	assert.Empty(t, q.kinds())
}

func TestIdempotentAcrossRestarts(t *testing.T) {
	// Two Scheduler instances SHARING one ledger + queue (a restart re-creates
	// the Scheduler but the durable ledger persists). The second instance must
	// NOT re-enqueue the day already claimed by the first.
	cal, err := calendar.NewNYSE()
	require.NoError(t, err)
	q := newFakeQueue()
	l := newFakeLedger()
	clk := newClock(at(t, 2026, time.June, 15, 18, 30))

	mk := func(instance string) *Scheduler {
		s, err := New(cal, q, l, zerolog.Nop(), Options{
			DailyAt: dailyAt1830(t), Loc: nyLoc(t), InstanceID: instance, Now: clk.now,
		})
		require.NoError(t, err)
		return s
	}

	s1 := mk("instance-1")
	res, err := s1.Tick(context.Background())
	require.NoError(t, err)
	require.Equal(t, ActionEnqueued, res.Action)

	// "Restart": a fresh Scheduler over the SAME ledger, same trading day.
	s2 := mk("instance-2")
	res, err = s2.Tick(context.Background())
	require.NoError(t, err)
	assert.Equal(t, ActionAlreadyDone, res.Action, "restart must not re-enqueue a claimed day")

	assert.Len(t, q.callsFor(handlers.KindDataRefresh), 1)
	assert.Len(t, q.callsFor(handlers.KindEODRefresh), 1)
	assert.Equal(t, 1, l.claimCount())
}

func TestConcurrentInstancesEnqueueOnce(t *testing.T) {
	// Many instances fire the same trading day at once; the ledger's
	// single-winner Claim must let exactly one enqueue the pipeline.
	cal, err := calendar.NewNYSE()
	require.NoError(t, err)
	q := newFakeQueue()
	l := newFakeLedger()
	clk := newClock(at(t, 2026, time.June, 15, 18, 30))

	const n = 16
	var wg sync.WaitGroup
	enqueued := make([]bool, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			s, err := New(cal, q, l, zerolog.Nop(), Options{
				DailyAt: dailyAt1830(t), Loc: nyLoc(t), Now: clk.now,
			})
			require.NoError(t, err)
			res, err := s.Tick(context.Background())
			require.NoError(t, err)
			enqueued[i] = res.Action == ActionEnqueued
		}(i)
	}
	wg.Wait()

	winners := 0
	for _, e := range enqueued {
		if e {
			winners++
		}
	}
	assert.Equal(t, 1, winners, "exactly one concurrent instance must win the day")
	assert.Len(t, q.callsFor(handlers.KindDataRefresh), 1)
	assert.Len(t, q.callsFor(handlers.KindEODRefresh), 1)
}

func TestCatchupEnqueuesMissedDayOnStartup(t *testing.T) {
	// Process starts at 22:00 ET on a trading day — the 18:30 fire time has
	// already passed and the day was never enqueued. With catch-up on, the
	// startup evaluation enqueues it once.
	clk := newClock(at(t, 2026, time.June, 15, 22, 0))
	s, q, l := newTestScheduler(t, clk, Options{
		DailyAt: dailyAt1830(t), Loc: nyLoc(t), Catchup: true,
	})

	res, err := s.evaluate(context.Background(), clk.now(), TriggerCatchup)
	require.NoError(t, err)
	require.Equal(t, ActionEnqueued, res.Action)
	assert.Len(t, q.callsFor(handlers.KindDataRefresh), 1)
	assert.Len(t, q.callsFor(handlers.KindEODRefresh), 1)
	assert.Equal(t, 1, l.claimCount())

	// A subsequent on-time tick the same day is a no-op (already claimed).
	res, err = s.Tick(context.Background())
	require.NoError(t, err)
	assert.Equal(t, ActionAlreadyDone, res.Action)
}

func TestCatchupBeforeFireTimeWaits(t *testing.T) {
	// Startup at 12:00 ET — BEFORE fire time. Catch-up must NOT pre-fire; the
	// pipeline waits for the real fire time.
	clk := newClock(at(t, 2026, time.June, 15, 12, 0))
	s, q, _ := newTestScheduler(t, clk, Options{
		DailyAt: dailyAt1830(t), Loc: nyLoc(t), Catchup: true,
	})
	res, err := s.evaluate(context.Background(), clk.now(), TriggerCatchup)
	require.NoError(t, err)
	assert.Equal(t, ActionWaiting, res.Action)
	assert.Empty(t, q.kinds())
}

func TestSyncNowForcesPipelineImmediately(t *testing.T) {
	// 09:00 ET on a trading day — well before the 18:30 fire time. SyncNow
	// forces the pipeline regardless, and is idempotent on repeat.
	clk := newClock(at(t, 2026, time.June, 15, 9, 0))
	s, q, l := newTestScheduler(t, clk, Options{DailyAt: dailyAt1830(t), Loc: nyLoc(t)})

	res, err := s.SyncNow(context.Background(), "cli")
	require.NoError(t, err)
	require.Equal(t, ActionEnqueued, res.Action)
	assert.Equal(t, calendar.NewDate(2026, time.June, 15), res.Date)
	assert.Len(t, q.callsFor(handlers.KindDataRefresh), 1)
	assert.Len(t, q.callsFor(handlers.KindEODRefresh), 1)

	// Second force same day: no-op.
	res, err = s.SyncNow(context.Background(), "cli")
	require.NoError(t, err)
	assert.Equal(t, ActionAlreadyDone, res.Action)
	assert.Len(t, q.callsFor(handlers.KindDataRefresh), 1)
	assert.Equal(t, 1, l.claimCount())
}

func TestSyncNowOnWeekendTargetsPriorSession(t *testing.T) {
	// Forced on a Saturday — targets Friday 2026-06-12, the prior session.
	clk := newClock(at(t, 2026, time.June, 13, 10, 0)) // Sat
	s, q, _ := newTestScheduler(t, clk, Options{DailyAt: dailyAt1830(t), Loc: nyLoc(t)})

	res, err := s.SyncNow(context.Background(), "api")
	require.NoError(t, err)
	require.Equal(t, ActionEnqueued, res.Action)
	assert.Equal(t, calendar.NewDate(2026, time.June, 12), res.Date, "weekend force targets prior session")
	eodCalls := q.callsFor(handlers.KindEODRefresh)
	require.Len(t, eodCalls, 1)
	assert.Equal(t, "2026-06-12", eodCalls[0].Payload.(map[string]any)["as_of"])
}

func TestRunStopsOnContextCancel(t *testing.T) {
	// The loop must exit promptly on cancellation (graceful shutdown).
	clk := newClock(at(t, 2026, time.June, 15, 9, 0))
	s, _, _ := newTestScheduler(t, clk, Options{
		DailyAt: dailyAt1830(t), Loc: nyLoc(t), Tick: 5 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop within 2s of cancel")
	}
}
