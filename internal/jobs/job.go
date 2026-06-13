package jobs

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Status is a job lifecycle state, matching the tms.jobs CHECK constraint.
// Note the database spelling "canceled" (single l).
type Status string

// Job lifecycle states.
const (
	StatusQueued    Status = "queued"
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
	StatusCanceled  Status = "canceled"
)

// Terminal reports whether the status is final (no further transitions).
func (s Status) Terminal() bool {
	switch s {
	case StatusSucceeded, StatusFailed, StatusCanceled:
		return true
	}
	return false
}

// Sentinel errors returned by Queue operations.
var (
	// ErrNoJob: Claim found no eligible queued job.
	ErrNoJob = errors.New("jobs: no claimable job")
	// ErrNotFound: the referenced job id does not exist.
	ErrNotFound = errors.New("jobs: job not found")
	// ErrLostClaim: the caller no longer owns the running job (it was
	// reaped, canceled or finished by someone else). The worker must stop
	// touching it.
	ErrLostClaim = errors.New("jobs: claim lost (job no longer running under this worker)")
	// ErrNoHandler: no handler is registered for the job kind.
	ErrNoHandler = errors.New("jobs: no handler registered for kind")
)

// Job mirrors one tms.jobs row. Pointer fields are NULLable columns.
type Job struct {
	ID              int64
	Kind            string
	Payload         json.RawMessage
	Status          Status
	Priority        int32
	RunAt           time.Time
	Attempts        int32
	MaxAttempts     int32
	DedupeKey       *string
	ClaimedBy       *string
	ClaimedAt       *time.Time
	HeartbeatAt     *time.Time
	StartedAt       *time.Time
	FinishedAt      *time.Time
	LastError       *string
	Progress        json.RawMessage
	CancelRequested bool
	Result          json.RawMessage
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// jobColumns is the canonical select list; keep in sync with scanJob.
const jobColumns = `id, kind, payload, status, priority, run_at, attempts, max_attempts,
	dedupe_key, claimed_by, claimed_at, heartbeat_at, started_at, finished_at,
	last_error, progress, cancel_requested, result, created_at, updated_at`

// scanJob scans one row in jobColumns order.
func scanJob(row pgx.Row) (*Job, error) {
	var j Job
	var status string
	err := row.Scan(
		&j.ID, &j.Kind, &j.Payload, &status, &j.Priority, &j.RunAt,
		&j.Attempts, &j.MaxAttempts,
		&j.DedupeKey, &j.ClaimedBy, &j.ClaimedAt, &j.HeartbeatAt,
		&j.StartedAt, &j.FinishedAt,
		&j.LastError, &j.Progress, &j.CancelRequested, &j.Result,
		&j.CreatedAt, &j.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	j.Status = Status(status)
	return &j, nil
}
