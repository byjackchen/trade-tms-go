package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
)

const (
	// maxErrorLen bounds last_error so a pathological handler error (or a
	// panic stack) cannot bloat the row.
	maxErrorLen = 8 << 10
	// defaultRetryBase/Cap shape the retry backoff: base * 2^(attempt-1),
	// capped. attempt is the count already consumed (>= 1 on first failure).
	defaultRetryBase = 30 * time.Second
	defaultRetryCap  = 15 * time.Minute
	// dedupeRaces bounds the insert/select retry loop in Enqueue when an
	// active duplicate finishes between the conflicting INSERT and the
	// follow-up SELECT.
	dedupeRaces = 3
)

// Queue is the durable job queue over tms.jobs. All methods are safe for
// concurrent use; every state change writes a tms.audit_log row in the same
// transaction and publishes a best-effort Event after commit.
type Queue struct {
	pool     *pgxpool.Pool
	log      zerolog.Logger
	notifier Notifier // nil = events disabled
	backoff  func(attempt int32) time.Duration
}

// Option customizes a Queue.
type Option func(*Queue)

// WithNotifier attaches a post-commit event publisher (nil disables events).
func WithNotifier(n Notifier) Option { return func(q *Queue) { q.notifier = n } }

// WithRetryBackoff overrides the retry delay function (attempt is the
// 1-based number of attempts already consumed).
func WithRetryBackoff(f func(attempt int32) time.Duration) Option {
	return func(q *Queue) { q.backoff = f }
}

// NewQueue builds a Queue over an existing pool.
func NewQueue(pool *pgxpool.Pool, log zerolog.Logger, opts ...Option) (*Queue, error) {
	if pool == nil {
		return nil, errors.New("jobs: nil connection pool")
	}
	q := &Queue{
		pool: pool,
		log:  log.With().Str("component", "jobs-queue").Logger(),
		backoff: func(attempt int32) time.Duration {
			d := defaultRetryBase
			for i := int32(1); i < attempt && d < defaultRetryCap; i++ {
				d *= 2
			}
			return min(d, defaultRetryCap)
		},
	}
	for _, o := range opts {
		o(q)
	}
	return q, nil
}

func (q *Queue) notify(ctx context.Context, ev Event) {
	if q.notifier == nil {
		return
	}
	ev.TS = time.Now().UTC()
	q.notifier.Notify(ctx, ev)
}

// audit appends one audit_log row inside the caller's transaction so the
// trail commits atomically with the state change it records.
func audit(ctx context.Context, tx pgx.Tx, actor, action string, jobID int64, details map[string]any) error {
	if actor == "" {
		actor = "system"
	}
	if details == nil {
		details = map[string]any{}
	}
	blob, err := json.Marshal(details)
	if err != nil {
		return fmt.Errorf("jobs: marshal audit details: %w", err)
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO tms.audit_log (actor, action, entity, entity_id, details)
		 VALUES ($1, $2, 'job', $3, $4)`,
		actor, action, strconv.FormatInt(jobID, 10), blob)
	if err != nil {
		return fmt.Errorf("jobs: insert audit row: %w", err)
	}
	return nil
}

func truncateErr(msg string) string {
	if len(msg) > maxErrorLen {
		return msg[:maxErrorLen] + "\n... (truncated)"
	}
	return msg
}

// marshalObject renders v as a JSONB-ready object. nil yields "{}".
// json.RawMessage / []byte pass through unchanged (caller guarantees an
// object — the column CHECK enforces it server-side anyway).
func marshalObject(v any) ([]byte, error) {
	switch t := v.(type) {
	case nil:
		return []byte("{}"), nil
	case json.RawMessage:
		return t, nil
	case []byte:
		return t, nil
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("jobs: marshal payload: %w", err)
		}
		return b, nil
	}
}

// ---------------------------------------------------------------------------
// Enqueue
// ---------------------------------------------------------------------------

// EnqueueParams describes one job submission.
type EnqueueParams struct {
	// Kind is the handler dispatch key (dotted lowercase), required.
	Kind string
	// Payload is the handler input; marshaled to a JSON object. nil = {}.
	Payload any
	// DedupeKey, when non-empty, guarantees at most one active
	// (queued|running) job with this key; duplicates return the existing
	// job with deduped=true.
	DedupeKey string
	// Priority orders claiming (higher first), default 0.
	Priority int32
	// RunAt is the earliest execution time; zero = now.
	RunAt time.Time
	// MaxAttempts caps total attempts (claim increments); <1 = 1.
	MaxAttempts int32
	// Actor is recorded in the audit trail ("" = "system").
	Actor string
}

// Enqueue inserts a job (or returns the existing active one when DedupeKey
// matches). The dedupe guarantee is the partial unique index
// jobs_dedupe_active_idx; the insert/select race when an active duplicate
// finishes mid-flight is resolved by retrying the insert.
func (q *Queue) Enqueue(ctx context.Context, p EnqueueParams) (job *Job, deduped bool, err error) {
	if p.Kind == "" {
		return nil, false, errors.New("jobs: enqueue: empty kind")
	}
	payload, err := marshalObject(p.Payload)
	if err != nil {
		return nil, false, err
	}
	maxAttempts := p.MaxAttempts
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	var runAt *time.Time
	if !p.RunAt.IsZero() {
		runAt = &p.RunAt
	}
	var dedupe *string
	if p.DedupeKey != "" {
		dedupe = &p.DedupeKey
	}

	for race := 0; race < dedupeRaces; race++ {
		job, err = q.tryInsert(ctx, p, payload, runAt, maxAttempts, dedupe)
		if err != nil {
			return nil, false, err
		}
		if job != nil {
			q.notify(ctx, Event{JobID: job.ID, Kind: job.Kind, Event: "enqueued", Status: job.Status})
			return job, false, nil
		}
		// Conflict: an active job with this dedupe key exists — fetch it.
		row := q.pool.QueryRow(ctx,
			`SELECT `+jobColumns+` FROM tms.jobs
			  WHERE dedupe_key = $1 AND status IN ('queued', 'running')
			  ORDER BY id DESC LIMIT 1`, p.DedupeKey)
		existing, serr := scanJob(row)
		if serr == nil {
			return existing, true, nil
		}
		if !errors.Is(serr, pgx.ErrNoRows) {
			return nil, false, fmt.Errorf("jobs: enqueue dedupe lookup: %w", serr)
		}
		// The duplicate finished between INSERT and SELECT; insert again.
	}
	return nil, false, fmt.Errorf("jobs: enqueue: dedupe race not settled after %d attempts (kind=%s)", dedupeRaces, p.Kind)
}

// tryInsert performs one INSERT ... ON CONFLICT DO NOTHING attempt plus its
// audit row; returns (nil, nil) on dedupe conflict.
func (q *Queue) tryInsert(ctx context.Context, p EnqueueParams, payload []byte, runAt *time.Time, maxAttempts int32, dedupe *string) (*Job, error) {
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("jobs: enqueue begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	row := tx.QueryRow(ctx,
		`INSERT INTO tms.jobs (kind, payload, priority, run_at, max_attempts, dedupe_key)
		 VALUES ($1, $2, $3, COALESCE($4, now()), $5, $6)
		 ON CONFLICT (dedupe_key) WHERE status IN ('queued', 'running') DO NOTHING
		 RETURNING `+jobColumns,
		p.Kind, payload, p.Priority, runAt, maxAttempts, dedupe)
	job, err := scanJob(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil // dedupe conflict
	}
	if err != nil {
		return nil, fmt.Errorf("jobs: enqueue insert: %w", err)
	}
	details := map[string]any{"kind": p.Kind, "priority": p.Priority, "max_attempts": maxAttempts}
	if dedupe != nil {
		details["dedupe_key"] = *dedupe
	}
	if err := audit(ctx, tx, p.Actor, "job.enqueued", job.ID, details); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("jobs: enqueue commit: %w", err)
	}
	return job, nil
}

// ---------------------------------------------------------------------------
// Claim / heartbeat / progress
// ---------------------------------------------------------------------------

// Claim atomically transitions the next eligible queued job to running
// under workerID using the canonical SKIP LOCKED statement from the 000006
// DDL header. Returns ErrNoJob when nothing is claimable.
func (q *Queue) Claim(ctx context.Context, workerID string) (*Job, error) {
	if workerID == "" {
		return nil, errors.New("jobs: claim: empty worker id")
	}
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("jobs: claim begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	row := tx.QueryRow(ctx,
		`UPDATE tms.jobs j
		    SET status = 'running', claimed_by = $1, claimed_at = now(),
		        started_at = now(), heartbeat_at = now(), attempts = attempts + 1
		  WHERE j.id = (
		        SELECT id FROM tms.jobs
		         WHERE status = 'queued' AND run_at <= now()
		         ORDER BY priority DESC, run_at, id
		         FOR UPDATE SKIP LOCKED
		         LIMIT 1)
		 RETURNING `+jobColumns, workerID)
	job, err := scanJob(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoJob
	}
	if err != nil {
		return nil, fmt.Errorf("jobs: claim: %w", err)
	}
	if err := audit(ctx, tx, workerID, "job.claimed", job.ID,
		map[string]any{"kind": job.Kind, "attempt": job.Attempts, "max_attempts": job.MaxAttempts}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("jobs: claim commit: %w", err)
	}
	q.notify(ctx, Event{JobID: job.ID, Kind: job.Kind, Event: "claimed", Status: StatusRunning, Worker: workerID})
	return job, nil
}

// Heartbeat bumps heartbeat_at for a running job owned by workerID and
// returns the cooperative cancel_requested flag. ErrLostClaim means the job
// is no longer running under this worker (reaped/canceled elsewhere): the
// worker must cancel the handler and write no terminal state.
func (q *Queue) Heartbeat(ctx context.Context, jobID int64, workerID string) (cancelRequested bool, err error) {
	row := q.pool.QueryRow(ctx,
		`UPDATE tms.jobs SET heartbeat_at = now()
		  WHERE id = $1 AND claimed_by = $2 AND status = 'running'
		 RETURNING cancel_requested`, jobID, workerID)
	if err := row.Scan(&cancelRequested); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, ErrLostClaim
		}
		return false, fmt.Errorf("jobs: heartbeat: %w", err)
	}
	return cancelRequested, nil
}

// ReportProgress stores the progress object (JSONB) on the running job,
// doubling as a heartbeat, and publishes a progress event. Like Heartbeat
// it returns cancel_requested / ErrLostClaim. Progress is deliberately not
// audited (cadence is too high for an append-only audit table).
func (q *Queue) ReportProgress(ctx context.Context, jobID int64, workerID string, progress any) (cancelRequested bool, err error) {
	blob, err := marshalObject(progress)
	if err != nil {
		return false, err
	}
	var kind string
	row := q.pool.QueryRow(ctx,
		`UPDATE tms.jobs SET progress = $3, heartbeat_at = now()
		  WHERE id = $1 AND claimed_by = $2 AND status = 'running'
		 RETURNING cancel_requested, kind`, jobID, workerID, blob)
	if err := row.Scan(&cancelRequested, &kind); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, ErrLostClaim
		}
		return false, fmt.Errorf("jobs: report progress: %w", err)
	}
	q.notify(ctx, Event{JobID: jobID, Kind: kind, Event: "progress", Status: StatusRunning, Worker: workerID, Progress: blob})
	return cancelRequested, nil
}

// ---------------------------------------------------------------------------
// Terminal transitions (owner-guarded)
// ---------------------------------------------------------------------------

// Succeed finishes a running job owned by workerID with an optional result
// object. ErrLostClaim when the guard fails.
func (q *Queue) Succeed(ctx context.Context, jobID int64, workerID string, result any) (*Job, error) {
	blob, err := marshalObject(result)
	if err != nil {
		return nil, err
	}
	return q.finishOwned(ctx, jobID, workerID, "job.succeeded", "succeeded",
		`UPDATE tms.jobs
		    SET status = 'succeeded', finished_at = now(), result = $3
		  WHERE id = $1 AND claimed_by = $2 AND status = 'running'
		 RETURNING `+jobColumns, blob)
}

// MarkCanceled finishes a running job owned by workerID as canceled (the
// cooperative-cancel completion path). reason lands in last_error.
func (q *Queue) MarkCanceled(ctx context.Context, jobID int64, workerID, reason string) (*Job, error) {
	return q.finishOwned(ctx, jobID, workerID, "job.canceled", "canceled",
		`UPDATE tms.jobs
		    SET status = 'canceled', finished_at = now(), last_error = NULLIF($3, '')
		  WHERE id = $1 AND claimed_by = $2 AND status = 'running'
		 RETURNING `+jobColumns, truncateErr(reason))
}

// Release returns a running job to the queue without consuming an attempt
// (worker drain: shutdown outlived DrainTimeout). The claim-time attempt
// increment is refunded.
func (q *Queue) Release(ctx context.Context, jobID int64, workerID, reason string) (*Job, error) {
	return q.finishOwned(ctx, jobID, workerID, "job.released", "released",
		`UPDATE tms.jobs
		    SET status = 'queued', run_at = now(),
		        attempts = GREATEST(attempts - 1, 0),
		        claimed_by = NULL, claimed_at = NULL, heartbeat_at = NULL,
		        started_at = NULL, last_error = NULLIF($3, '')
		  WHERE id = $1 AND claimed_by = $2 AND status = 'running'
		 RETURNING `+jobColumns, truncateErr(reason))
}

// finishOwned runs one owner-guarded transition + audit + event.
func (q *Queue) finishOwned(ctx context.Context, jobID int64, workerID, action, event, sql string, arg any) (*Job, error) {
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("jobs: %s begin: %w", action, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	job, err := scanJob(tx.QueryRow(ctx, sql, jobID, workerID, arg))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrLostClaim
	}
	if err != nil {
		return nil, fmt.Errorf("jobs: %s: %w", action, err)
	}
	details := map[string]any{"kind": job.Kind, "attempt": job.Attempts}
	if job.LastError != nil {
		details["reason"] = *job.LastError
	}
	if err := audit(ctx, tx, workerID, action, job.ID, details); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("jobs: %s commit: %w", action, err)
	}
	ev := Event{JobID: job.ID, Kind: job.Kind, Event: event, Status: job.Status, Worker: workerID}
	if job.LastError != nil {
		ev.Error = *job.LastError
	}
	q.notify(ctx, ev)
	return job, nil
}

// Fail records a handler failure on a running job owned by workerID. With
// attempts remaining the job is re-queued with exponential backoff
// (event/audit "requeued"); otherwise it goes terminal failed. jobErr is
// captured in last_error either way (truncated to 8 KiB).
func (q *Queue) Fail(ctx context.Context, jobID int64, workerID, jobErr string) (*Job, error) {
	if jobErr == "" {
		jobErr = "unknown error"
	}
	jobErr = truncateErr(jobErr)

	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("jobs: fail begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var attempts, maxAttempts int32
	err = tx.QueryRow(ctx,
		`SELECT attempts, max_attempts FROM tms.jobs
		  WHERE id = $1 AND claimed_by = $2 AND status = 'running'
		  FOR UPDATE`, jobID, workerID).Scan(&attempts, &maxAttempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrLostClaim
	}
	if err != nil {
		return nil, fmt.Errorf("jobs: fail lock: %w", err)
	}

	var (
		job    *Job
		action string
		event  string
	)
	if attempts < maxAttempts {
		delay := q.backoff(attempts)
		job, err = scanJob(tx.QueryRow(ctx,
			`UPDATE tms.jobs
			    SET status = 'queued', run_at = now() + make_interval(secs => $3),
			        claimed_by = NULL, claimed_at = NULL, heartbeat_at = NULL,
			        started_at = NULL, last_error = $4
			  WHERE id = $1 AND claimed_by = $2 AND status = 'running'
			 RETURNING `+jobColumns, jobID, workerID, delay.Seconds(), jobErr))
		action, event = "job.requeued", "requeued"
	} else {
		job, err = scanJob(tx.QueryRow(ctx,
			`UPDATE tms.jobs
			    SET status = 'failed', finished_at = now(), last_error = $3
			  WHERE id = $1 AND claimed_by = $2 AND status = 'running'
			 RETURNING `+jobColumns, jobID, workerID, jobErr))
		action, event = "job.failed", "failed"
	}
	if err != nil {
		return nil, fmt.Errorf("jobs: fail update: %w", err)
	}
	details := map[string]any{
		"kind": job.Kind, "attempt": attempts, "max_attempts": maxAttempts, "error": jobErr,
	}
	if event == "requeued" {
		details["next_run_at"] = job.RunAt.UTC().Format(time.RFC3339)
	}
	if err := audit(ctx, tx, workerID, action, job.ID, details); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("jobs: fail commit: %w", err)
	}
	q.notify(ctx, Event{JobID: job.ID, Kind: job.Kind, Event: event, Status: job.Status, Worker: workerID, Error: jobErr})
	return job, nil
}

// ---------------------------------------------------------------------------
// Cancel
// ---------------------------------------------------------------------------

// CancelOutcome describes what Cancel did.
type CancelOutcome string

// Cancel outcomes.
const (
	// CancelDone: the job was queued and is now terminally canceled.
	CancelDone CancelOutcome = "canceled"
	// CancelRequested: the job is running; the cooperative flag was set
	// (idempotent — repeat cancels return the same outcome).
	CancelRequested CancelOutcome = "cancel_requested"
	// CancelAlreadyTerminal: the job had already finished; no-op.
	CancelAlreadyTerminal CancelOutcome = "already_terminal"
)

// Cancel cancels a job. Queued jobs finish immediately as canceled; running
// jobs get cancel_requested set, observed by the owning worker on its next
// heartbeat/progress round-trip (cooperative cancellation). Terminal jobs
// are a no-op. Returns ErrNotFound for unknown ids.
func (q *Queue) Cancel(ctx context.Context, jobID int64, actor, reason string) (CancelOutcome, *Job, error) {
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("jobs: cancel begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	job, err := scanJob(tx.QueryRow(ctx,
		`SELECT `+jobColumns+` FROM tms.jobs WHERE id = $1 FOR UPDATE`, jobID))
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, fmt.Errorf("%w: id=%d", ErrNotFound, jobID)
	}
	if err != nil {
		return "", nil, fmt.Errorf("jobs: cancel lock: %w", err)
	}

	if reason == "" {
		reason = "canceled by " + nonEmpty(actor, "system")
	}
	reason = truncateErr(reason)

	switch job.Status {
	case StatusQueued:
		job, err = scanJob(tx.QueryRow(ctx,
			`UPDATE tms.jobs
			    SET status = 'canceled', finished_at = now(), last_error = $2
			  WHERE id = $1
			 RETURNING `+jobColumns, jobID, reason))
		if err != nil {
			return "", nil, fmt.Errorf("jobs: cancel queued: %w", err)
		}
		if err := audit(ctx, tx, actor, "job.canceled", jobID,
			map[string]any{"kind": job.Kind, "reason": reason, "was": "queued"}); err != nil {
			return "", nil, err
		}
		if err := tx.Commit(ctx); err != nil {
			return "", nil, fmt.Errorf("jobs: cancel commit: %w", err)
		}
		q.notify(ctx, Event{JobID: job.ID, Kind: job.Kind, Event: "canceled", Status: StatusCanceled, Error: reason})
		return CancelDone, job, nil

	case StatusRunning:
		alreadyRequested := job.CancelRequested
		job, err = scanJob(tx.QueryRow(ctx,
			`UPDATE tms.jobs SET cancel_requested = true
			  WHERE id = $1
			 RETURNING `+jobColumns, jobID))
		if err != nil {
			return "", nil, fmt.Errorf("jobs: cancel running: %w", err)
		}
		if !alreadyRequested { // audit/notify the first request only
			if err := audit(ctx, tx, actor, "job.cancel_requested", jobID,
				map[string]any{"kind": job.Kind, "reason": reason}); err != nil {
				return "", nil, err
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return "", nil, fmt.Errorf("jobs: cancel commit: %w", err)
		}
		if !alreadyRequested {
			q.notify(ctx, Event{JobID: job.ID, Kind: job.Kind, Event: "cancel_requested", Status: StatusRunning, Error: reason})
		}
		return CancelRequested, job, nil

	default: // terminal — no-op
		return CancelAlreadyTerminal, job, nil
	}
}

func nonEmpty(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// ---------------------------------------------------------------------------
// Stale-claim recovery
// ---------------------------------------------------------------------------

// ReapStats summarizes one ReapStale pass.
type ReapStats struct {
	Requeued int // worker died, attempts remain → queued again
	Failed   int // worker died, attempts exhausted → failed
	Canceled int // worker died after a cancel request → canceled
}

// Total is the number of stale jobs acted upon.
func (s ReapStats) Total() int { return s.Requeued + s.Failed + s.Canceled }

// ReapStale recovers running jobs whose heartbeat is older than staleAfter
// (the claiming worker died or lost its DB connection). Per job:
// cancel_requested → canceled; attempts >= max_attempts → failed; otherwise
// → re-queued for another worker. Rows are locked with SKIP LOCKED so
// concurrent reapers (every worker runs one) cooperate safely; batches of
// reapBatch repeat until the backlog is drained.
func (q *Queue) ReapStale(ctx context.Context, staleAfter time.Duration) (ReapStats, error) {
	var stats ReapStats
	if staleAfter <= 0 {
		return stats, errors.New("jobs: reap: staleAfter must be > 0")
	}
	const reapBatch = 100
	for {
		n, batch, err := q.reapBatch(ctx, staleAfter, reapBatch)
		stats.Requeued += batch.Requeued
		stats.Failed += batch.Failed
		stats.Canceled += batch.Canceled
		if err != nil {
			return stats, err
		}
		if n < reapBatch {
			return stats, nil
		}
	}
}

func (q *Queue) reapBatch(ctx context.Context, staleAfter time.Duration, limit int) (int, ReapStats, error) {
	var stats ReapStats
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return 0, stats, fmt.Errorf("jobs: reap begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx,
		`SELECT id, kind, attempts, max_attempts, cancel_requested, claimed_by
		   FROM tms.jobs
		  WHERE status = 'running'
		    AND heartbeat_at < now() - make_interval(secs => $1)
		  ORDER BY heartbeat_at
		  FOR UPDATE SKIP LOCKED
		  LIMIT $2`, staleAfter.Seconds(), limit)
	if err != nil {
		return 0, stats, fmt.Errorf("jobs: reap select: %w", err)
	}
	type stale struct {
		id              int64
		kind            string
		attempts        int32
		maxAttempts     int32
		cancelRequested bool
		claimedBy       *string
	}
	var found []stale
	for rows.Next() {
		var s stale
		if err := rows.Scan(&s.id, &s.kind, &s.attempts, &s.maxAttempts, &s.cancelRequested, &s.claimedBy); err != nil {
			rows.Close()
			return 0, stats, fmt.Errorf("jobs: reap scan: %w", err)
		}
		found = append(found, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, stats, fmt.Errorf("jobs: reap rows: %w", err)
	}
	if len(found) == 0 {
		return 0, stats, tx.Commit(ctx)
	}

	var events []Event
	for _, s := range found {
		deadWorker := "unknown"
		if s.claimedBy != nil {
			deadWorker = *s.claimedBy
		}
		reason := fmt.Sprintf("worker heartbeat expired (claimed_by=%s)", deadWorker)
		details := map[string]any{
			"kind": s.kind, "attempt": s.attempts, "max_attempts": s.maxAttempts,
			"dead_worker": deadWorker,
		}
		switch {
		case s.cancelRequested:
			_, err = tx.Exec(ctx,
				`UPDATE tms.jobs
				    SET status = 'canceled', finished_at = now(), last_error = $2
				  WHERE id = $1`, s.id, "cancel requested; "+reason)
			if err == nil {
				err = audit(ctx, tx, "reaper", "job.canceled", s.id, details)
			}
			stats.Canceled++
			events = append(events, Event{JobID: s.id, Kind: s.kind, Event: "canceled", Status: StatusCanceled, Error: reason})
		case s.attempts >= s.maxAttempts:
			_, err = tx.Exec(ctx,
				`UPDATE tms.jobs
				    SET status = 'failed', finished_at = now(), last_error = $2
				  WHERE id = $1`, s.id, reason)
			if err == nil {
				err = audit(ctx, tx, "reaper", "job.failed", s.id, details)
			}
			stats.Failed++
			events = append(events, Event{JobID: s.id, Kind: s.kind, Event: "failed", Status: StatusFailed, Error: reason})
		default:
			_, err = tx.Exec(ctx,
				`UPDATE tms.jobs
				    SET status = 'queued', run_at = now(),
				        claimed_by = NULL, claimed_at = NULL, heartbeat_at = NULL,
				        started_at = NULL, last_error = $2
				  WHERE id = $1`, s.id, reason)
			if err == nil {
				err = audit(ctx, tx, "reaper", "job.reaped", s.id, details)
			}
			stats.Requeued++
			events = append(events, Event{JobID: s.id, Kind: s.kind, Event: "reaped", Status: StatusQueued, Error: reason})
		}
		if err != nil {
			return 0, ReapStats{}, fmt.Errorf("jobs: reap job %d: %w", s.id, err)
		}
		q.log.Warn().Int64("job_id", s.id).Str("kind", s.kind).
			Str("dead_worker", deadWorker).
			Int32("attempt", s.attempts).Int32("max_attempts", s.maxAttempts).
			Bool("cancel_requested", s.cancelRequested).
			Msg("reaped stale running job")
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, ReapStats{}, fmt.Errorf("jobs: reap commit: %w", err)
	}
	for _, ev := range events {
		q.notify(ctx, ev)
	}
	return len(found), stats, nil
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

// Get fetches one job by id (ErrNotFound when absent).
func (q *Queue) Get(ctx context.Context, jobID int64) (*Job, error) {
	job, err := scanJob(q.pool.QueryRow(ctx,
		`SELECT `+jobColumns+` FROM tms.jobs WHERE id = $1`, jobID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: id=%d", ErrNotFound, jobID)
	}
	if err != nil {
		return nil, fmt.Errorf("jobs: get: %w", err)
	}
	return job, nil
}

// ListFilter narrows List output. Zero values mean "no filter".
type ListFilter struct {
	Kind   string
	Status Status
	Limit  int32 // <=0 → 50
}

// List returns jobs newest-first for ops inspection (CLI / API).
func (q *Queue) List(ctx context.Context, f ListFilter) ([]*Job, error) {
	if f.Limit <= 0 {
		f.Limit = 50
	}
	rows, err := q.pool.Query(ctx,
		`SELECT `+jobColumns+` FROM tms.jobs
		  WHERE ($1 = '' OR kind = $1)
		    AND ($2 = '' OR status = $2)
		  ORDER BY id DESC
		  LIMIT $3`, f.Kind, string(f.Status), f.Limit)
	if err != nil {
		return nil, fmt.Errorf("jobs: list: %w", err)
	}
	defer rows.Close()
	var out []*Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("jobs: list scan: %w", err)
		}
		out = append(out, j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("jobs: list rows: %w", err)
	}
	return out, nil
}
