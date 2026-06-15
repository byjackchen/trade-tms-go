package scheduler

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
)

// Trigger labels why a daily run was enqueued (mirrors the scheduler_runs
// CHECK constraint).
type Trigger string

// Trigger values.
const (
	// TriggerScheduled: the on-time daily tick fired the run.
	TriggerScheduled Trigger = "scheduled"
	// TriggerCatchup: startup found a trading day whose configured time had
	// already passed with no prior run, and enqueued it late.
	TriggerCatchup Trigger = "catchup"
	// TriggerManual: forced via `tms sync now` / POST /api/v1/data/sync-now.
	TriggerManual Trigger = "manual"
)

// PipelineDaily is the only scheduled pipeline today; the column keeps the
// ledger open to future cadences without a migration.
const PipelineDaily = "daily"

// ClaimResult reports the outcome of a Claim attempt.
type ClaimResult struct {
	// Won is true when THIS caller inserted the (pipeline, trading_date) slot
	// (it must now enqueue the pipeline); false means another instance or an
	// earlier tick already owns the slot (no-op).
	Won bool
	// RunID is the scheduler_runs row id on a win (0 when Won is false).
	RunID int64
}

// Ledger is the durable single-leader claim store for daily runs. Claim is
// the dedupe primitive: exactly one caller per (pipeline, trading_date) gets
// Won=true. The interface lets the unit tests substitute an in-memory fake
// (controllable clock, no database) while production / the integration test
// use the PG-backed implementation.
type Ledger interface {
	// Claim atomically attempts to own the (pipeline, trading_date) slot for
	// claimedBy with the given trigger. Idempotent: a second call for the same
	// slot returns Won=false.
	Claim(ctx context.Context, pipeline string, tradingDate calendar.Date, claimedBy string, trig Trigger) (ClaimResult, error)
	// RecordJobs annotates a won slot with the enqueued pipeline job ids
	// (best-effort traceability; a failure here never un-claims the slot).
	RecordJobs(ctx context.Context, runID int64, dataJobID, eodJobID int64) error
}

// PGLedger is the tms.scheduler_runs-backed Ledger.
type PGLedger struct {
	pool *pgxpool.Pool
}

// NewPGLedger builds a PGLedger over an existing pool.
func NewPGLedger(pool *pgxpool.Pool) (*PGLedger, error) {
	if pool == nil {
		return nil, errors.New("scheduler: nil connection pool")
	}
	return &PGLedger{pool: pool}, nil
}

// Claim implements Ledger using INSERT ... ON CONFLICT DO NOTHING against the
// scheduler_runs_slot_idx unique index. RETURNING yields the new id only when
// the insert wins; a conflict returns no rows (Won=false).
func (l *PGLedger) Claim(ctx context.Context, pipeline string, tradingDate calendar.Date, claimedBy string, trig Trigger) (ClaimResult, error) {
	if pipeline == "" {
		return ClaimResult{}, errors.New("scheduler: claim: empty pipeline")
	}
	if claimedBy == "" {
		return ClaimResult{}, errors.New("scheduler: claim: empty claimed_by")
	}
	var id int64
	err := l.pool.QueryRow(ctx,
		`INSERT INTO tms.scheduler_runs (pipeline, trading_date, claimed_by, trigger)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (pipeline, trading_date) DO NOTHING
		 RETURNING id`,
		pipeline, tradingDate.String(), claimedBy, string(trig)).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ClaimResult{Won: false}, nil
	}
	if err != nil {
		return ClaimResult{}, fmt.Errorf("scheduler: claim slot (%s, %s): %w", pipeline, tradingDate, err)
	}
	return ClaimResult{Won: true, RunID: id}, nil
}

// RecordJobs implements Ledger.
func (l *PGLedger) RecordJobs(ctx context.Context, runID int64, dataJobID, eodJobID int64) error {
	_, err := l.pool.Exec(ctx,
		`UPDATE tms.scheduler_runs SET data_job_id = $2, eod_job_id = $3 WHERE id = $1`,
		runID, nullableJobID(dataJobID), nullableJobID(eodJobID))
	if err != nil {
		return fmt.Errorf("scheduler: record jobs for run %d: %w", runID, err)
	}
	return nil
}

// nullableJobID renders a non-positive job id as SQL NULL.
func nullableJobID(id int64) *int64 {
	if id <= 0 {
		return nil
	}
	return &id
}
