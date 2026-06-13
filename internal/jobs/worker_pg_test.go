package jobs_test

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/jobs"
)

// fastOpts returns worker options tuned for test latency while keeping the
// heartbeat <= staleAfter/3 invariant.
func fastOpts(id string, concurrency int, drain time.Duration) jobs.WorkerOptions {
	return jobs.WorkerOptions{
		ID:                id,
		Concurrency:       concurrency,
		PollInterval:      30 * time.Millisecond,
		HeartbeatInterval: 40 * time.Millisecond,
		StaleAfter:        2 * time.Second,
		ReapInterval:      200 * time.Millisecond,
		DrainTimeout:      drain,
	}
}

// startWorker runs w.Run in a goroutine; the returned stop() cancels and
// waits for the drain to complete.
func startWorker(t *testing.T, w *jobs.Worker) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	stopped := false
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(15 * time.Second):
			t.Fatal("worker did not drain within 15s")
		}
	}
	t.Cleanup(stop)
	return stop
}

func TestWorkerExecutesJobsWithProgressAndResult(t *testing.T) {
	q := newTestQueue(t)
	ctx := testCtx(t)

	var ran atomic.Int32
	reg := jobs.NewRegistry()
	reg.MustRegister(jobs.HandlerFunc{K: "test.ok", F: func(hctx context.Context, job *jobs.Job, report jobs.ProgressFn) (any, error) {
		var p struct {
			N int `json:"n"`
		}
		require.NoError(t, json.Unmarshal(job.Payload, &p))
		require.NoError(t, report(hctx, map[string]any{"step": "halfway"}))
		ran.Add(1)
		return map[string]any{"n_times_2": p.N * 2}, nil
	}})

	w, err := jobs.NewWorker(q, reg, zerolog.Nop(), fastOpts("w-exec", 2, time.Second))
	require.NoError(t, err)
	startWorker(t, w)

	var ids []int64
	for i := 1; i <= 3; i++ {
		job, _, err := q.Enqueue(ctx, jobs.EnqueueParams{Kind: "test.ok", Payload: map[string]any{"n": i}})
		require.NoError(t, err)
		ids = append(ids, job.ID)
	}
	for i, id := range ids {
		job := waitStatus(t, ctx, q, id, jobs.StatusSucceeded, 10*time.Second)
		var res map[string]any
		require.NoError(t, json.Unmarshal(job.Result, &res))
		assert.EqualValues(t, (i+1)*2, res["n_times_2"])
		var prog map[string]any
		require.NoError(t, json.Unmarshal(job.Progress, &prog))
		assert.Equal(t, "halfway", prog["step"])
	}
	assert.EqualValues(t, 3, ran.Load())
}

// TestWorkerPanicIsolation: a panicking handler fails its job with the
// stack captured, and the executor survives to run the next job.
func TestWorkerPanicIsolation(t *testing.T) {
	q := newTestQueue(t)
	ctx := testCtx(t)

	reg := jobs.NewRegistry()
	reg.MustRegister(jobs.HandlerFunc{K: "test.panic", F: func(context.Context, *jobs.Job, jobs.ProgressFn) (any, error) {
		panic("kaboom")
	}})
	reg.MustRegister(jobs.HandlerFunc{K: "test.ok", F: func(context.Context, *jobs.Job, jobs.ProgressFn) (any, error) {
		return nil, nil
	}})

	w, err := jobs.NewWorker(q, reg, zerolog.Nop(), fastOpts("w-panic", 1, time.Second))
	require.NoError(t, err)
	startWorker(t, w)

	// Higher priority: the panicking job runs first on the single executor.
	panicJob, _, err := q.Enqueue(ctx, jobs.EnqueueParams{Kind: "test.panic", Priority: 10, MaxAttempts: 1})
	require.NoError(t, err)
	okJob, _, err := q.Enqueue(ctx, jobs.EnqueueParams{Kind: "test.ok"})
	require.NoError(t, err)

	failed := waitStatus(t, ctx, q, panicJob.ID, jobs.StatusFailed, 10*time.Second)
	require.NotNil(t, failed.LastError)
	assert.Contains(t, *failed.LastError, "handler panic: kaboom")
	assert.Contains(t, *failed.LastError, "goroutine", "stack trace captured")

	waitStatus(t, ctx, q, okJob.ID, jobs.StatusSucceeded, 10*time.Second)
}

// TestWorkerUnknownKind: with attempts exhausted an unroutable job goes
// terminal failed (ret-with-backoff is covered by Fail semantics).
func TestWorkerUnknownKind(t *testing.T) {
	q := newTestQueue(t, jobs.WithRetryBackoff(func(int32) time.Duration { return 0 }))
	ctx := testCtx(t)

	w, err := jobs.NewWorker(q, jobs.NewRegistry(), zerolog.Nop(), fastOpts("w-unknown", 1, time.Second))
	require.NoError(t, err)
	startWorker(t, w)

	job, _, err := q.Enqueue(ctx, jobs.EnqueueParams{Kind: "nobody.home", MaxAttempts: 2})
	require.NoError(t, err)
	failed := waitStatus(t, ctx, q, job.ID, jobs.StatusFailed, 10*time.Second)
	assert.EqualValues(t, 2, failed.Attempts)
	require.NotNil(t, failed.LastError)
	assert.Contains(t, *failed.LastError, "no handler registered")
}

// TestWorkerCooperativeCancel: Cancel on a running job flips the flag, the
// heartbeat relays it, the handler's context is canceled and the job lands
// terminally canceled.
func TestWorkerCooperativeCancel(t *testing.T) {
	q := newTestQueue(t)
	ctx := testCtx(t)

	entered := make(chan struct{}, 1)
	reg := jobs.NewRegistry()
	reg.MustRegister(jobs.HandlerFunc{K: "test.block", F: func(hctx context.Context, _ *jobs.Job, _ jobs.ProgressFn) (any, error) {
		entered <- struct{}{}
		<-hctx.Done() // a well-behaved handler: stop when told
		return nil, hctx.Err()
	}})

	w, err := jobs.NewWorker(q, reg, zerolog.Nop(), fastOpts("w-cancel", 1, time.Second))
	require.NoError(t, err)
	startWorker(t, w)

	job, _, err := q.Enqueue(ctx, jobs.EnqueueParams{Kind: "test.block"})
	require.NoError(t, err)
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("handler never started")
	}

	outcome, _, err := q.Cancel(ctx, job.ID, "ops", "operator cancel")
	require.NoError(t, err)
	require.Equal(t, jobs.CancelRequested, outcome)

	canceled := waitStatus(t, ctx, q, job.ID, jobs.StatusCanceled, 10*time.Second)
	require.NotNil(t, canceled.LastError)
	assert.Contains(t, *canceled.LastError, "context canceled")
	actions := auditActions(t, ctx, job.ID)
	assert.Equal(t,
		[]string{"job.enqueued", "job.claimed", "job.cancel_requested", "job.canceled"},
		actions)
}

// TestWorkerDrainReleasesOverrunningJob: SIGTERM-equivalent shutdown gives
// the handler DrainTimeout, then cancels it and releases the job back to
// the queue with its attempt refunded.
func TestWorkerDrainReleasesOverrunningJob(t *testing.T) {
	q := newTestQueue(t)
	ctx := testCtx(t)

	entered := make(chan struct{}, 1)
	reg := jobs.NewRegistry()
	reg.MustRegister(jobs.HandlerFunc{K: "test.block", F: func(hctx context.Context, _ *jobs.Job, _ jobs.ProgressFn) (any, error) {
		entered <- struct{}{}
		<-hctx.Done()
		return nil, hctx.Err()
	}})

	w, err := jobs.NewWorker(q, reg, zerolog.Nop(), fastOpts("w-drain", 1, 150*time.Millisecond))
	require.NoError(t, err)
	stop := startWorker(t, w)

	job, _, err := q.Enqueue(ctx, jobs.EnqueueParams{Kind: "test.block", MaxAttempts: 1})
	require.NoError(t, err)
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("handler never started")
	}

	stop() // shutdown: drain expires after 150ms, job must be released

	released := waitStatus(t, ctx, q, job.ID, jobs.StatusQueued, 10*time.Second)
	assert.EqualValues(t, 0, released.Attempts, "attempt refunded on drain release")
	assert.Nil(t, released.ClaimedBy)
	require.NotNil(t, released.LastError)
	assert.Contains(t, *released.LastError, "drain timeout")
}

// TestWorkerFinishesInFlightJobWithinDrain: handlers that finish inside
// DrainTimeout complete normally during shutdown.
func TestWorkerFinishesInFlightJobWithinDrain(t *testing.T) {
	q := newTestQueue(t)
	ctx := testCtx(t)

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	reg := jobs.NewRegistry()
	reg.MustRegister(jobs.HandlerFunc{K: "test.slow", F: func(hctx context.Context, _ *jobs.Job, _ jobs.ProgressFn) (any, error) {
		entered <- struct{}{}
		select {
		case <-release:
			return map[string]any{"finished": true}, nil
		case <-hctx.Done():
			return nil, hctx.Err()
		}
	}})

	w, err := jobs.NewWorker(q, reg, zerolog.Nop(), fastOpts("w-drain-ok", 1, 10*time.Second))
	require.NoError(t, err)
	stop := startWorker(t, w)

	job, _, err := q.Enqueue(ctx, jobs.EnqueueParams{Kind: "test.slow"})
	require.NoError(t, err)
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("handler never started")
	}

	// Begin shutdown, then let the handler finish well within DrainTimeout.
	go func() {
		time.Sleep(100 * time.Millisecond)
		close(release)
	}()
	stop()

	done := waitStatus(t, ctx, q, job.ID, jobs.StatusSucceeded, 10*time.Second)
	var res map[string]any
	require.NoError(t, json.Unmarshal(done.Result, &res))
	assert.Equal(t, true, res["finished"])
}
