package scheduler

import (
	"context"
	"fmt"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/jobs"
	"github.com/byjackchen/trade-tms-go/internal/jobs/handlers"
)

// Enqueuer is the slice of jobs.Queue the scheduler uses (its Enqueue
// method). The seam lets the scheduler's pipeline + tick logic be unit-tested
// against a recording fake with no database; *jobs.Queue satisfies it.
type Enqueuer interface {
	Enqueue(ctx context.Context, p jobs.EnqueueParams) (*jobs.Job, bool, error)
}

// PipelineResult is the outcome of enqueuing one trading day's pipeline.
type PipelineResult struct {
	// DataJobID is the enqueued data.refresh job id.
	DataJobID int64
	// EODJobID is the enqueued eod.refresh job id.
	EODJobID int64
	// DataDeduped / EODDeduped report whether the active-job dedupe index
	// returned an already-active job instead of inserting a new one (a
	// concurrent manual refresh, say). The scheduler-run slot is still the
	// authoritative once-per-day guarantee; these flags are informational.
	DataDeduped bool
	EODDeduped  bool
}

// eodGraceAfterData floors the eod.refresh run_at this far after the
// data.refresh is enqueued, giving the data catchup a head start so the EOD
// replay sees the freshest bars. Correctness does not depend on it (eod is
// idempotent and the worker claims data.refresh first by priority), but it
// avoids replaying stale bars on the common path.
const eodGraceAfterData = 10 * time.Minute

// dataRefreshPriority makes the data.refresh job claim ahead of the
// eod.refresh job when both are queued (higher priority claims first).
const dataRefreshPriority = 10

// enqueuePipeline enqueues the daily data pipeline for tradingDate as the
// given actor: (a) data.refresh source=api (incremental Sharadar catchup),
// then (b) eod.refresh as_of=tradingDate (signal-intent precompute), run_at-
// floored so it follows the data refresh. The per-day single-enqueue
// guarantee is enforced by the caller's ledger Claim; the jobs themselves
// carry day-scoped dedupe keys as a second line of defence.
func enqueuePipeline(ctx context.Context, q Enqueuer, tradingDate calendar.Date, now time.Time, actor string) (PipelineResult, error) {
	var res PipelineResult

	dataJob, dataDeduped, err := q.Enqueue(ctx, jobs.EnqueueParams{
		Kind:        handlers.KindDataRefresh,
		Payload:     map[string]any{"source": "api"},
		DedupeKey:   fmt.Sprintf("scheduler:data.refresh:%s", tradingDate),
		Priority:    dataRefreshPriority,
		MaxAttempts: 3,
		Actor:       actor,
	})
	if err != nil {
		return res, fmt.Errorf("scheduler: enqueue data.refresh for %s: %w", tradingDate, err)
	}
	res.DataJobID = dataJob.ID
	res.DataDeduped = dataDeduped

	eodJob, eodDeduped, err := q.Enqueue(ctx, jobs.EnqueueParams{
		Kind:        handlers.KindEODRefresh,
		Payload:     map[string]any{"as_of": tradingDate.String(), "strategy": "multi"},
		DedupeKey:   fmt.Sprintf("scheduler:eod.refresh:%s", tradingDate),
		RunAt:       now.Add(eodGraceAfterData),
		MaxAttempts: 3,
		Actor:       actor,
	})
	if err != nil {
		// The data.refresh is already enqueued (and the slot claimed); surface
		// the eod failure so the operator/health can see the partial pipeline.
		return res, fmt.Errorf("scheduler: enqueue eod.refresh for %s: %w", tradingDate, err)
	}
	res.EODJobID = eodJob.ID
	res.EODDeduped = eodDeduped
	return res, nil
}
