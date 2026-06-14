//go:build integration

package jobs_test

// Integration-test harness: claim/heartbeat/cancel semantics are verified
// against a REAL PostgreSQL (FOR UPDATE SKIP LOCKED, partial unique indexes and
// make_interval cannot be faked meaningfully). The ephemeral-container bootstrap
// lives in internal/testutil/pgtest (shared with runner/runs/study/api); only
// the jobs-specific TRUNCATE list and per-test helpers stay here.

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/jobs"
	"github.com/byjackchen/trade-tms-go/internal/testutil/pgtest"
)

func TestMain(m *testing.M) {
	os.Exit(pgtest.Run(m, "jobs"))
}

// ---------------------------------------------------------------------------
// Per-test helpers
// ---------------------------------------------------------------------------

// recNotifier records published events for assertions.
type recNotifier struct {
	mu     sync.Mutex
	events []jobs.Event
}

func (r *recNotifier) Notify(_ context.Context, ev jobs.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *recNotifier) names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.events))
	for i, ev := range r.events {
		out[i] = ev.Event
	}
	return out
}

// newTestQueue skips without docker, truncates job/audit state and builds
// a Queue. Integration tests share one database, so they must not run in
// parallel (none call t.Parallel()).
func newTestQueue(t *testing.T, opts ...jobs.Option) *jobs.Queue {
	t.Helper()
	pool := pgtest.RequirePG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := pool.Exec(ctx, `TRUNCATE tms.jobs, tms.audit_log RESTART IDENTITY`)
	require.NoError(t, err)
	q, err := jobs.NewQueue(pool, zerolog.Nop(), opts...)
	require.NoError(t, err)
	return q
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// ageHeartbeat backdates a running job's heartbeat to simulate a dead
// worker.
func ageHeartbeat(t *testing.T, ctx context.Context, jobID int64, age time.Duration) {
	t.Helper()
	_, err := pgtest.RequirePG(t).Exec(ctx,
		`UPDATE tms.jobs SET heartbeat_at = now() - make_interval(secs => $2) WHERE id = $1`,
		jobID, age.Seconds())
	require.NoError(t, err)
}

// auditActions returns the audit_log action sequence for one job.
func auditActions(t *testing.T, ctx context.Context, jobID int64) []string {
	t.Helper()
	rows, err := pgtest.RequirePG(t).Query(ctx,
		`SELECT action FROM tms.audit_log WHERE entity = 'job' AND entity_id = $1 ORDER BY id`,
		fmt.Sprint(jobID))
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		require.NoError(t, rows.Scan(&a))
		out = append(out, a)
	}
	require.NoError(t, rows.Err())
	return out
}

// waitStatus polls until the job reaches want (worker tests).
func waitStatus(t *testing.T, ctx context.Context, q *jobs.Queue, jobID int64, want jobs.Status, within time.Duration) *jobs.Job {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		job, err := q.Get(ctx, jobID)
		require.NoError(t, err)
		if job.Status == want {
			return job
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %d never reached %s within %s (now %s, last_error=%v)",
				jobID, want, within, job.Status, job.LastError)
		}
		time.Sleep(25 * time.Millisecond)
	}
}
