package jobs_test

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/jobs"
)

func TestEnqueueGetAndAudit(t *testing.T) {
	rec := &recNotifier{}
	q := newTestQueue(t, jobs.WithNotifier(rec))
	ctx := testCtx(t)

	job, deduped, err := q.Enqueue(ctx, jobs.EnqueueParams{
		Kind:        "data.refresh",
		Payload:     map[string]any{"source": "parquet", "tables": []string{"sep"}},
		MaxAttempts: 3,
		Priority:    7,
		Actor:       "test",
	})
	require.NoError(t, err)
	assert.False(t, deduped)
	assert.Equal(t, jobs.StatusQueued, job.Status)
	assert.EqualValues(t, 0, job.Attempts)
	assert.EqualValues(t, 3, job.MaxAttempts)
	assert.EqualValues(t, 7, job.Priority)

	got, err := q.Get(ctx, job.ID)
	require.NoError(t, err)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(got.Payload, &payload))
	assert.Equal(t, "parquet", payload["source"])

	assert.Equal(t, []string{"job.enqueued"}, auditActions(t, ctx, job.ID))
	assert.Equal(t, []string{"enqueued"}, rec.names())

	_, err = q.Get(ctx, 99999)
	require.ErrorIs(t, err, jobs.ErrNotFound)
}

func TestEnqueueDedupe(t *testing.T) {
	q := newTestQueue(t)
	ctx := testCtx(t)

	first, deduped, err := q.Enqueue(ctx, jobs.EnqueueParams{Kind: "k", DedupeKey: "k:daily"})
	require.NoError(t, err)
	require.False(t, deduped)

	// Second active enqueue with the same key returns the first job.
	second, deduped, err := q.Enqueue(ctx, jobs.EnqueueParams{Kind: "k", DedupeKey: "k:daily"})
	require.NoError(t, err)
	assert.True(t, deduped)
	assert.Equal(t, first.ID, second.ID)

	// Dedupe holds while the job is RUNNING too.
	_, err = q.Claim(ctx, "w1")
	require.NoError(t, err)
	_, deduped, err = q.Enqueue(ctx, jobs.EnqueueParams{Kind: "k", DedupeKey: "k:daily"})
	require.NoError(t, err)
	assert.True(t, deduped)

	// Terminal state frees the key.
	_, err = q.Succeed(ctx, first.ID, "w1", nil)
	require.NoError(t, err)
	third, deduped, err := q.Enqueue(ctx, jobs.EnqueueParams{Kind: "k", DedupeKey: "k:daily"})
	require.NoError(t, err)
	assert.False(t, deduped)
	assert.NotEqual(t, first.ID, third.ID)

	// Different keys never collide.
	_, deduped, err = q.Enqueue(ctx, jobs.EnqueueParams{Kind: "k", DedupeKey: "k:other"})
	require.NoError(t, err)
	assert.False(t, deduped)
}

func TestClaimOrderingAndRunAt(t *testing.T) {
	q := newTestQueue(t)
	ctx := testCtx(t)

	_, err := q.Claim(ctx, "w1")
	require.ErrorIs(t, err, jobs.ErrNoJob, "empty queue must return ErrNoJob")

	lo, _, err := q.Enqueue(ctx, jobs.EnqueueParams{Kind: "k", Priority: 0})
	require.NoError(t, err)
	hi, _, err := q.Enqueue(ctx, jobs.EnqueueParams{Kind: "k", Priority: 10})
	require.NoError(t, err)
	_, _, err = q.Enqueue(ctx, jobs.EnqueueParams{
		Kind: "k", Priority: 99, RunAt: time.Now().Add(time.Hour), // future: not claimable
	})
	require.NoError(t, err)

	first, err := q.Claim(ctx, "w1")
	require.NoError(t, err)
	assert.Equal(t, hi.ID, first.ID, "higher priority claims first")
	assert.Equal(t, jobs.StatusRunning, first.Status)
	assert.EqualValues(t, 1, first.Attempts)
	require.NotNil(t, first.ClaimedBy)
	assert.Equal(t, "w1", *first.ClaimedBy)
	assert.NotNil(t, first.HeartbeatAt)

	second, err := q.Claim(ctx, "w2")
	require.NoError(t, err)
	assert.Equal(t, lo.ID, second.ID)

	// Only the future-dated job remains; nothing claimable now.
	_, err = q.Claim(ctx, "w3")
	require.ErrorIs(t, err, jobs.ErrNoJob)
}

// TestClaimConcurrentSkipLocked proves no double-delivery under concurrent
// claimers hammering the same queue (the SKIP LOCKED contract).
func TestClaimConcurrentSkipLocked(t *testing.T) {
	q := newTestQueue(t)
	ctx := testCtx(t)

	const n = 12
	for i := 0; i < n; i++ {
		_, _, err := q.Enqueue(ctx, jobs.EnqueueParams{Kind: "k"})
		require.NoError(t, err)
	}

	var (
		mu      sync.Mutex
		claimed = map[int64]string{}
		wg      sync.WaitGroup
	)
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(worker string) {
			defer wg.Done()
			for {
				job, err := q.Claim(ctx, worker)
				if err != nil {
					if !errors.Is(err, jobs.ErrNoJob) {
						t.Errorf("claim error: %v", err)
					}
					return // drained
				}
				mu.Lock()
				prev, dup := claimed[job.ID]
				claimed[job.ID] = worker
				mu.Unlock()
				if dup {
					t.Errorf("job %d claimed twice (%s then %s)", job.ID, prev, worker)
				}
			}
		}(string(rune('a' + w)))
	}
	wg.Wait()
	assert.Len(t, claimed, n, "every job claimed exactly once")
}

func TestHeartbeatOwnershipAndCancelFlag(t *testing.T) {
	q := newTestQueue(t)
	ctx := testCtx(t)

	job, _, err := q.Enqueue(ctx, jobs.EnqueueParams{Kind: "k"})
	require.NoError(t, err)

	_, err = q.Heartbeat(ctx, job.ID, "w1")
	require.ErrorIs(t, err, jobs.ErrLostClaim, "heartbeat on a queued job is a lost claim")

	_, err = q.Claim(ctx, "w1")
	require.NoError(t, err)

	cr, err := q.Heartbeat(ctx, job.ID, "w1")
	require.NoError(t, err)
	assert.False(t, cr)

	_, err = q.Heartbeat(ctx, job.ID, "impostor")
	require.ErrorIs(t, err, jobs.ErrLostClaim, "only the claiming worker may heartbeat")

	// Cooperative cancel: the flag travels back on the next heartbeat.
	outcome, _, err := q.Cancel(ctx, job.ID, "test", "")
	require.NoError(t, err)
	assert.Equal(t, jobs.CancelRequested, outcome)
	cr, err = q.Heartbeat(ctx, job.ID, "w1")
	require.NoError(t, err)
	assert.True(t, cr)

	// Worker honors it.
	done, err := q.MarkCanceled(ctx, job.ID, "w1", "canceled: context canceled")
	require.NoError(t, err)
	assert.Equal(t, jobs.StatusCanceled, done.Status)
	assert.NotNil(t, done.FinishedAt)
	assert.Equal(t,
		[]string{"job.enqueued", "job.claimed", "job.cancel_requested", "job.canceled"},
		auditActions(t, ctx, job.ID))
}

func TestReportProgress(t *testing.T) {
	rec := &recNotifier{}
	q := newTestQueue(t, jobs.WithNotifier(rec))
	ctx := testCtx(t)

	job, _, err := q.Enqueue(ctx, jobs.EnqueueParams{Kind: "k"})
	require.NoError(t, err)
	_, err = q.Claim(ctx, "w1")
	require.NoError(t, err)

	cr, err := q.ReportProgress(ctx, job.ID, "w1", map[string]any{"rows": 42})
	require.NoError(t, err)
	assert.False(t, cr)

	got, err := q.Get(ctx, job.ID)
	require.NoError(t, err)
	var prog map[string]any
	require.NoError(t, json.Unmarshal(got.Progress, &prog))
	assert.EqualValues(t, 42, prog["rows"])

	_, err = q.ReportProgress(ctx, job.ID, "impostor", map[string]any{})
	require.ErrorIs(t, err, jobs.ErrLostClaim)

	assert.Contains(t, rec.names(), "progress")
	// Progress is intentionally not audited.
	assert.Equal(t, []string{"job.enqueued", "job.claimed"}, auditActions(t, ctx, job.ID))
}

func TestSucceedStoresResult(t *testing.T) {
	q := newTestQueue(t)
	ctx := testCtx(t)

	job, _, err := q.Enqueue(ctx, jobs.EnqueueParams{Kind: "k"})
	require.NoError(t, err)
	_, err = q.Claim(ctx, "w1")
	require.NoError(t, err)

	done, err := q.Succeed(ctx, job.ID, "w1", map[string]any{"rows_upserted": 7})
	require.NoError(t, err)
	assert.Equal(t, jobs.StatusSucceeded, done.Status)
	require.NotNil(t, done.FinishedAt)
	var res map[string]any
	require.NoError(t, json.Unmarshal(done.Result, &res))
	assert.EqualValues(t, 7, res["rows_upserted"])

	// Terminal: a late impostor/duplicate write is a lost claim.
	_, err = q.Succeed(ctx, job.ID, "w1", nil)
	require.ErrorIs(t, err, jobs.ErrLostClaim)
}

func TestFailRetriesWithBackoffThenFailsTerminally(t *testing.T) {
	// Zero backoff so the retry is immediately claimable in the test.
	q := newTestQueue(t, jobs.WithRetryBackoff(func(int32) time.Duration { return 0 }))
	ctx := testCtx(t)

	job, _, err := q.Enqueue(ctx, jobs.EnqueueParams{Kind: "k", MaxAttempts: 2})
	require.NoError(t, err)

	// Attempt 1 fails → requeued with error captured.
	_, err = q.Claim(ctx, "w1")
	require.NoError(t, err)
	after, err := q.Fail(ctx, job.ID, "w1", "boom 1")
	require.NoError(t, err)
	assert.Equal(t, jobs.StatusQueued, after.Status)
	assert.Nil(t, after.ClaimedBy)
	require.NotNil(t, after.LastError)
	assert.Equal(t, "boom 1", *after.LastError)

	// Attempt 2 fails → terminal failed.
	claimed, err := q.Claim(ctx, "w2")
	require.NoError(t, err)
	assert.Equal(t, job.ID, claimed.ID)
	assert.EqualValues(t, 2, claimed.Attempts)
	final, err := q.Fail(ctx, job.ID, "w2", "boom 2")
	require.NoError(t, err)
	assert.Equal(t, jobs.StatusFailed, final.Status)
	require.NotNil(t, final.LastError)
	assert.Equal(t, "boom 2", *final.LastError)
	require.NotNil(t, final.FinishedAt)

	assert.Equal(t,
		[]string{"job.enqueued", "job.claimed", "job.requeued", "job.claimed", "job.failed"},
		auditActions(t, ctx, job.ID))
}

func TestFailBackoffSchedulesFuture(t *testing.T) {
	q := newTestQueue(t) // default exponential backoff (30s base)
	ctx := testCtx(t)

	job, _, err := q.Enqueue(ctx, jobs.EnqueueParams{Kind: "k", MaxAttempts: 3})
	require.NoError(t, err)
	_, err = q.Claim(ctx, "w1")
	require.NoError(t, err)
	requeued, err := q.Fail(ctx, job.ID, "w1", "transient")
	require.NoError(t, err)
	assert.Equal(t, jobs.StatusQueued, requeued.Status)
	assert.True(t, requeued.RunAt.After(time.Now().Add(20*time.Second)),
		"retry must be scheduled with backoff, got run_at=%s", requeued.RunAt)

	// And therefore not immediately claimable.
	_, err = q.Claim(ctx, "w1")
	require.ErrorIs(t, err, jobs.ErrNoJob)
}

func TestCancelQueuedAndTerminal(t *testing.T) {
	q := newTestQueue(t)
	ctx := testCtx(t)

	job, _, err := q.Enqueue(ctx, jobs.EnqueueParams{Kind: "k"})
	require.NoError(t, err)

	outcome, got, err := q.Cancel(ctx, job.ID, "ops", "not needed")
	require.NoError(t, err)
	assert.Equal(t, jobs.CancelDone, outcome)
	assert.Equal(t, jobs.StatusCanceled, got.Status)
	require.NotNil(t, got.LastError)
	assert.Equal(t, "not needed", *got.LastError)

	// Canceling again is a no-op, not an error.
	outcome, _, err = q.Cancel(ctx, job.ID, "ops", "")
	require.NoError(t, err)
	assert.Equal(t, jobs.CancelAlreadyTerminal, outcome)

	_, _, err = q.Cancel(ctx, 424242, "ops", "")
	require.ErrorIs(t, err, jobs.ErrNotFound)
}

func TestReapStaleRecovery(t *testing.T) {
	q := newTestQueue(t)
	ctx := testCtx(t)

	// (a) attempts remain → requeued and claimable by another worker.
	requeue, _, err := q.Enqueue(ctx, jobs.EnqueueParams{Kind: "k", MaxAttempts: 2})
	require.NoError(t, err)
	// (b) attempts exhausted → failed.
	exhausted, _, err := q.Enqueue(ctx, jobs.EnqueueParams{Kind: "k", MaxAttempts: 1})
	require.NoError(t, err)
	// (c) cancel requested before the worker died → canceled.
	wantCancel, _, err := q.Enqueue(ctx, jobs.EnqueueParams{Kind: "k", MaxAttempts: 2})
	require.NoError(t, err)
	// (d) healthy running job → untouched.
	healthy, _, err := q.Enqueue(ctx, jobs.EnqueueParams{Kind: "k", MaxAttempts: 2})
	require.NoError(t, err)

	for range 4 {
		_, err := q.Claim(ctx, "dead-worker")
		require.NoError(t, err)
	}
	outcome, _, err := q.Cancel(ctx, wantCancel.ID, "ops", "")
	require.NoError(t, err)
	require.Equal(t, jobs.CancelRequested, outcome)

	for _, id := range []int64{requeue.ID, exhausted.ID, wantCancel.ID} {
		ageHeartbeat(t, ctx, id, 10*time.Minute)
	}

	stats, err := q.ReapStale(ctx, time.Minute)
	require.NoError(t, err)
	assert.Equal(t, jobs.ReapStats{Requeued: 1, Failed: 1, Canceled: 1}, stats)

	gotRequeue, err := q.Get(ctx, requeue.ID)
	require.NoError(t, err)
	assert.Equal(t, jobs.StatusQueued, gotRequeue.Status)
	assert.Nil(t, gotRequeue.ClaimedBy)
	require.NotNil(t, gotRequeue.LastError)
	assert.Contains(t, *gotRequeue.LastError, "heartbeat expired")
	assert.Contains(t, *gotRequeue.LastError, "dead-worker")

	gotExhausted, err := q.Get(ctx, exhausted.ID)
	require.NoError(t, err)
	assert.Equal(t, jobs.StatusFailed, gotExhausted.Status)

	gotCanceled, err := q.Get(ctx, wantCancel.ID)
	require.NoError(t, err)
	assert.Equal(t, jobs.StatusCanceled, gotCanceled.Status)

	gotHealthy, err := q.Get(ctx, healthy.ID)
	require.NoError(t, err)
	assert.Equal(t, jobs.StatusRunning, gotHealthy.Status, "fresh heartbeat must not be reaped")

	// The dead worker's late heartbeat/write is rejected.
	_, err = q.Heartbeat(ctx, requeue.ID, "dead-worker")
	require.ErrorIs(t, err, jobs.ErrLostClaim)
	_, err = q.Succeed(ctx, exhausted.ID, "dead-worker", nil)
	require.ErrorIs(t, err, jobs.ErrLostClaim)

	// Reclaim the requeued job: attempts keep counting across incarnations.
	reclaimed, err := q.Claim(ctx, "w2")
	require.NoError(t, err)
	assert.Equal(t, requeue.ID, reclaimed.ID)
	assert.EqualValues(t, 2, reclaimed.Attempts)
}

func TestReleaseRefundsAttempt(t *testing.T) {
	q := newTestQueue(t)
	ctx := testCtx(t)

	job, _, err := q.Enqueue(ctx, jobs.EnqueueParams{Kind: "k", MaxAttempts: 1})
	require.NoError(t, err)
	_, err = q.Claim(ctx, "w1")
	require.NoError(t, err)

	released, err := q.Release(ctx, job.ID, "w1", "released: drain")
	require.NoError(t, err)
	assert.Equal(t, jobs.StatusQueued, released.Status)
	assert.EqualValues(t, 0, released.Attempts, "drain release must refund the attempt")

	// Still claimable with its full attempt budget.
	re, err := q.Claim(ctx, "w2")
	require.NoError(t, err)
	assert.Equal(t, job.ID, re.ID)
	assert.EqualValues(t, 1, re.Attempts)
}

func TestList(t *testing.T) {
	q := newTestQueue(t)
	ctx := testCtx(t)

	for i := 0; i < 3; i++ {
		_, _, err := q.Enqueue(ctx, jobs.EnqueueParams{Kind: "a"})
		require.NoError(t, err)
	}
	_, _, err := q.Enqueue(ctx, jobs.EnqueueParams{Kind: "b"})
	require.NoError(t, err)
	_, err = q.Claim(ctx, "w1")
	require.NoError(t, err)

	all, err := q.List(ctx, jobs.ListFilter{})
	require.NoError(t, err)
	assert.Len(t, all, 4)
	assert.True(t, all[0].ID > all[1].ID, "newest first")

	onlyB, err := q.List(ctx, jobs.ListFilter{Kind: "b"})
	require.NoError(t, err)
	assert.Len(t, onlyB, 1)

	running, err := q.List(ctx, jobs.ListFilter{Status: jobs.StatusRunning})
	require.NoError(t, err)
	assert.Len(t, running, 1)

	limited, err := q.List(ctx, jobs.ListFilter{Limit: 2})
	require.NoError(t, err)
	assert.Len(t, limited, 2)
}
