//go:build integration

package scheduler_test

// Integration harness: the single-leader dedupe guarantee of
// tms.scheduler_runs is a PARTIAL/UNIQUE-index + INSERT ON CONFLICT property
// that cannot be faked — it must be exercised against a REAL migrated
// PostgreSQL. We verify: a single Claim wins; a repeat for the same
// (pipeline, trading_date) loses; concurrent claimers yield exactly one
// winner; distinct dates each win; and RecordJobs persists the job ids.

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/scheduler"
	"github.com/byjackchen/trade-tms-go/internal/testutil/pgtest"
)

func TestMain(m *testing.M) {
	os.Exit(pgtest.Run(m, "scheduler"))
}

func newLedger(t *testing.T) *scheduler.PGLedger {
	t.Helper()
	pool := pgtest.RequirePG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := pool.Exec(ctx, `TRUNCATE tms.scheduler_runs RESTART IDENTITY`)
	require.NoError(t, err)
	l, err := scheduler.NewPGLedger(pool)
	require.NoError(t, err)
	return l
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestPGLedgerClaimDedupesPerTradingDate(t *testing.T) {
	l := newLedger(t)
	ctx := testCtx(t)
	d := calendar.NewDate(2026, time.June, 15)

	first, err := l.Claim(ctx, scheduler.PipelineDaily, d, "instance-1", scheduler.TriggerScheduled)
	require.NoError(t, err)
	assert.True(t, first.Won, "first claim must win")
	assert.Positive(t, first.RunID)

	// Same slot again — even from a different instance / trigger — loses.
	second, err := l.Claim(ctx, scheduler.PipelineDaily, d, "instance-2", scheduler.TriggerCatchup)
	require.NoError(t, err)
	assert.False(t, second.Won, "second claim for the same trading date must lose")
	assert.Zero(t, second.RunID)

	// A different trading date is an independent slot — wins.
	other, err := l.Claim(ctx, scheduler.PipelineDaily, d.AddDays(1), "instance-1", scheduler.TriggerScheduled)
	require.NoError(t, err)
	assert.True(t, other.Won)
	assert.NotEqual(t, first.RunID, other.RunID)
}

func TestPGLedgerConcurrentClaimSingleWinner(t *testing.T) {
	l := newLedger(t)
	ctx := testCtx(t)
	d := calendar.NewDate(2026, time.June, 16)

	const n = 24
	var wins int64
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			res, err := l.Claim(ctx, scheduler.PipelineDaily, d, "racer", scheduler.TriggerScheduled)
			assert.NoError(t, err)
			if res.Won {
				atomic.AddInt64(&wins, 1)
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, int64(1), atomic.LoadInt64(&wins), "exactly one concurrent claimer must win the slot")

	// And the table holds exactly one row for that slot.
	var count int
	require.NoError(t, pgtest.RequirePG(t).QueryRow(ctx,
		`SELECT count(*) FROM tms.scheduler_runs WHERE pipeline = $1 AND trading_date = $2`,
		scheduler.PipelineDaily, d.String()).Scan(&count))
	assert.Equal(t, 1, count)
}

func TestPGLedgerRecordJobsPersists(t *testing.T) {
	l := newLedger(t)
	ctx := testCtx(t)
	d := calendar.NewDate(2026, time.June, 17)

	claim, err := l.Claim(ctx, scheduler.PipelineDaily, d, "instance-1", scheduler.TriggerManual)
	require.NoError(t, err)
	require.True(t, claim.Won)

	require.NoError(t, l.RecordJobs(ctx, claim.RunID, 101, 202))

	var (
		dataJob, eodJob int64
		trig            string
	)
	require.NoError(t, pgtest.RequirePG(t).QueryRow(ctx,
		`SELECT data_job_id, eod_job_id, trigger FROM tms.scheduler_runs WHERE id = $1`, claim.RunID).
		Scan(&dataJob, &eodJob, &trig))
	assert.Equal(t, int64(101), dataJob)
	assert.Equal(t, int64(202), eodJob)
	assert.Equal(t, "manual", trig)
}
