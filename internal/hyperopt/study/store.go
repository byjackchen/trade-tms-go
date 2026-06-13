package study

// store.go is the PostgreSQL persistence for hyperopt studies — the DB source of
// truth alongside the legacy artifact tree (locked decision 3). It implements the
// Sink interface (study + trial + progress upserts streamed as trials complete)
// and the read side backing the API (list / detail / trials). It writes
// research.hyperopt_studies (study.json + progress.json folded into one row) and
// research.hyperopt_trials (trial_%04d.json equivalents). Upserts are idempotent
// on (study_ts) / (study_ts, number), so a re-run with the same study_ts and seed
// converges instead of duplicating.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/byjackchen/trade-tms-go/internal/runs"
)

// ErrStudyNotFound is returned for an unknown study_ts.
var ErrStudyNotFound = errors.New("hyperopt: study not found")

// Store persists and reads hyperopt studies/trials.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore wraps a pgx pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// compile-time checks: Store is a Sink and a ResumeSource.
var (
	_ Sink         = (*Store)(nil)
	_ ResumeSource = (*Store)(nil)
)

// UpsertStudy writes/updates the study row (identity + config + live progress).
func (s *Store) UpsertStudy(ctx context.Context, cfg StudyConfig, p Progress) error {
	cb, err := currentBestJSON(p.CurrentBest)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO tms.hyperopt_studies
		    (study_ts, study_name, strategy, start_date, end_date,
		     directions, objectives, seed, n_trials, workers,
		     walk_forward, folds, embargo_days, dump_trials, trial_timeout_sec,
		     status, completed_trials, failed_trials, running_trials,
		     started_at, last_heartbeat_at, coordinator_pid, current_best, last_error)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24)
		ON CONFLICT (study_ts) DO UPDATE SET
		    study_name = EXCLUDED.study_name,
		    strategy = EXCLUDED.strategy,
		    start_date = EXCLUDED.start_date,
		    end_date = EXCLUDED.end_date,
		    directions = EXCLUDED.directions,
		    objectives = EXCLUDED.objectives,
		    seed = EXCLUDED.seed,
		    n_trials = EXCLUDED.n_trials,
		    workers = EXCLUDED.workers,
		    walk_forward = EXCLUDED.walk_forward,
		    folds = EXCLUDED.folds,
		    embargo_days = EXCLUDED.embargo_days,
		    dump_trials = EXCLUDED.dump_trials,
		    trial_timeout_sec = EXCLUDED.trial_timeout_sec,
		    status = EXCLUDED.status,
		    completed_trials = EXCLUDED.completed_trials,
		    failed_trials = EXCLUDED.failed_trials,
		    running_trials = EXCLUDED.running_trials,
		    started_at = EXCLUDED.started_at,
		    last_heartbeat_at = EXCLUDED.last_heartbeat_at,
		    coordinator_pid = EXCLUDED.coordinator_pid,
		    current_best = EXCLUDED.current_best,
		    last_error = EXCLUDED.last_error`,
		cfg.StudyTS(), cfg.StudyName, cfg.Strategy, cfg.Start, cfg.End,
		cfg.Directions, cfg.Objectives, cfg.Seed, cfg.NTrials, cfg.Workers,
		cfg.WalkForward.Enabled, cfg.WalkForward.Folds, cfg.WalkForward.EmbargoDays, true, cfg.TrialTimeoutSec,
		string(p.Status), p.CompletedTrials, p.FailedTrials, p.RunningTrials,
		p.StartedAt, p.LastHeartbeatAt, p.CoordinatorPID, cb, p.LastError,
	)
	if err != nil {
		return fmt.Errorf("hyperopt: upsert study %s: %w", cfg.StudyTS(), err)
	}
	return nil
}

// StudyTS derives the study_ts from the study name's suffix (the dir is the ts).
// The coordinator passes cfg with StudyName "hyperopt-<strategy>-<ts>"; the ts is
// the trailing %Y-%m-%d_%H-%M-%S. We expose it on StudyConfig for the store.
func (c StudyConfig) StudyTS() string {
	// study_name = "hyperopt-<strategy>-<ts>"; the ts is the last 19 chars
	// (YYYY-MM-DD_HH-MM-SS) — strategy names contain no such pattern.
	const tsLen = 19
	if len(c.StudyName) >= tsLen {
		return c.StudyName[len(c.StudyName)-tsLen:]
	}
	return c.StudyName
}

// Progress updates only the live-progress columns of an existing study row.
func (s *Store) Progress(ctx context.Context, studyTS string, p Progress) error {
	cb, err := currentBestJSON(p.CurrentBest)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		UPDATE tms.hyperopt_studies SET
		    status = $2, completed_trials = $3, failed_trials = $4, running_trials = $5,
		    started_at = $6, last_heartbeat_at = $7, coordinator_pid = $8,
		    current_best = $9, last_error = $10
		WHERE study_ts = $1`,
		studyTS, string(p.Status), p.CompletedTrials, p.FailedTrials, p.RunningTrials,
		p.StartedAt, p.LastHeartbeatAt, p.CoordinatorPID, cb, p.LastError,
	)
	if err != nil {
		return fmt.Errorf("hyperopt: update progress %s: %w", studyTS, err)
	}
	return nil
}

// Heartbeat stamps ONLY last_heartbeat_at (and updated_at via the row trigger)
// on the study row (spec §6.10). Every other live-progress column is left
// untouched. A no-op when the row does not exist yet.
func (s *Store) Heartbeat(ctx context.Context, studyTS string, now time.Time) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE tms.hyperopt_studies SET last_heartbeat_at = $2 WHERE study_ts = $1`,
		studyTS, now.UTC())
	if err != nil {
		return fmt.Errorf("hyperopt: heartbeat %s: %w", studyTS, err)
	}
	return nil
}

// UpsertTrial writes one trial row, idempotent on (study_ts, number).
func (s *Store) UpsertTrial(ctx context.Context, studyTS string, t TrialArtifact) error {
	paramsJSON, err := json.Marshal(nonNilMap(t.Params))
	if err != nil {
		return fmt.Errorf("hyperopt: marshal trial params: %w", err)
	}
	metricsJSON := []byte("{}")
	var sharpe, calmar *float64
	if t.State == TrialComplete {
		metricsJSON, err = json.Marshal(t.Metrics)
		if err != nil {
			return fmt.Errorf("hyperopt: marshal trial metrics: %w", err)
		}
		sh, cl := t.Metrics.Sharpe, t.Metrics.Calmar
		sharpe, calmar = &sh, &cl
	}
	foldsJSON, err := foldsToJSON(t.Folds)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO tms.hyperopt_trials
		    (study_ts, number, optuna_number, strategy, params, metrics, folds,
		     state, sharpe, calmar, started_at, finished_at, duration_sec, run_dump_ts, error)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
		ON CONFLICT (study_ts, number) DO UPDATE SET
		    optuna_number = EXCLUDED.optuna_number,
		    strategy = EXCLUDED.strategy,
		    params = EXCLUDED.params,
		    metrics = EXCLUDED.metrics,
		    folds = EXCLUDED.folds,
		    state = EXCLUDED.state,
		    sharpe = EXCLUDED.sharpe,
		    calmar = EXCLUDED.calmar,
		    started_at = EXCLUDED.started_at,
		    finished_at = EXCLUDED.finished_at,
		    duration_sec = EXCLUDED.duration_sec,
		    run_dump_ts = EXCLUDED.run_dump_ts,
		    error = EXCLUDED.error`,
		studyTS, t.Number, t.OptunaNumber, t.Strategy, paramsJSON, metricsJSON, foldsJSON,
		string(t.State), sharpe, calmar, t.StartedAt, t.FinishedAt, t.DurationS, t.RunDumpTS, t.Error,
	)
	if err != nil {
		return fmt.Errorf("hyperopt: upsert trial %s/%d: %w", studyTS, t.Number, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// read side
// ---------------------------------------------------------------------------

// StudyRow is one row of the study list / detail (study config + live progress).
type StudyRow struct {
	StudyConfig
	TS              string
	Status          Status
	CompletedTrials int
	FailedTrials    int
	RunningTrials   int
	StartedAt       *time.Time
	LastHeartbeatAt *time.Time
	CoordinatorPID  *int
	CurrentBest     *CurrentBest
	LastError       *string
}

// TrialRow is one trial as read back (params/metrics/folds as raw JSON).
type TrialRow struct {
	Number       int
	OptunaNumber *int
	Strategy     string
	Params       json.RawMessage
	Metrics      json.RawMessage
	Folds        json.RawMessage
	State        TrialState
	Sharpe       *float64
	Calmar       *float64
	StartedAt    time.Time
	FinishedAt   *time.Time
	DurationS    float64
	RunDumpTS    *string
	Error        *string
}

// List returns studies newest-first (study_ts descending), optionally filtered
// by strategy ("" = any), limited.
func (s *Store) List(ctx context.Context, strategy string, limit int) ([]StudyRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT study_ts, study_name, strategy, start_date::text, end_date::text,
		       directions, objectives, seed, n_trials, workers,
		       walk_forward, folds, embargo_days, created_at, updated_at,
		       status, completed_trials, failed_trials, running_trials,
		       started_at, last_heartbeat_at, coordinator_pid, current_best, last_error
		FROM tms.hyperopt_studies
		WHERE ($1 = '' OR strategy = $1)
		ORDER BY study_ts DESC
		LIMIT $2`, strategy, limit)
	if err != nil {
		return nil, fmt.Errorf("hyperopt: list studies: %w", err)
	}
	defer rows.Close()
	var out []StudyRow
	now := time.Now()
	for rows.Next() {
		r, err := scanStudyRow(rows)
		if err != nil {
			return nil, err
		}
		// Present a stale zombie RUNNING study as INTERRUPTED (§9.2); the stored
		// row is unchanged.
		applyStaleness(&r, now)
		out = append(out, r)
	}
	return out, rows.Err()
}

// Get returns one study by ts, or ErrStudyNotFound.
func (s *Store) Get(ctx context.Context, studyTS string) (*StudyRow, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT study_ts, study_name, strategy, start_date::text, end_date::text,
		       directions, objectives, seed, n_trials, workers,
		       walk_forward, folds, embargo_days, created_at, updated_at,
		       status, completed_trials, failed_trials, running_trials,
		       started_at, last_heartbeat_at, coordinator_pid, current_best, last_error
		FROM tms.hyperopt_studies WHERE study_ts = $1`, studyTS)
	r, err := scanStudyRow(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrStudyNotFound
	}
	if err != nil {
		return nil, err
	}
	// §9.2 staleness override: a RUNNING study whose heartbeat is stale (>60s)
	// AND whose coordinator PID is nil or not alive is PRESENTED as INTERRUPTED
	// (the stored row is NOT modified). Catches a crashed/killed coordinator
	// that left the study permanently RUNNING (zombie study).
	applyStaleness(&r, time.Now())
	return &r, nil
}

// Trials returns all trials of a study in ascending number order.
func (s *Store) Trials(ctx context.Context, studyTS string) ([]TrialRow, error) {
	// Confirm the study exists for a clean 404.
	var exists bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM tms.hyperopt_studies WHERE study_ts = $1)`, studyTS).Scan(&exists); err != nil {
		return nil, fmt.Errorf("hyperopt: trials existence check: %w", err)
	}
	if !exists {
		return nil, ErrStudyNotFound
	}
	rows, err := s.pool.Query(ctx, `
		SELECT number, optuna_number, strategy, params, metrics, folds, state,
		       sharpe, calmar, started_at, finished_at, duration_sec, run_dump_ts, error
		FROM tms.hyperopt_trials WHERE study_ts = $1 ORDER BY number ASC`, studyTS)
	if err != nil {
		return nil, fmt.Errorf("hyperopt: list trials: %w", err)
	}
	defer rows.Close()
	var out []TrialRow
	for rows.Next() {
		var t TrialRow
		if err := rows.Scan(&t.Number, &t.OptunaNumber, &t.Strategy, &t.Params, &t.Metrics,
			&t.Folds, &t.State, &t.Sharpe, &t.Calmar, &t.StartedAt, &t.FinishedAt,
			&t.DurationS, &t.RunDumpTS, &t.Error); err != nil {
			return nil, fmt.Errorf("hyperopt: scanning trial: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// CompletedTrial is one already-COMPLETE trial's resume payload: its objective
// values plus the artifact/sampler numbers needed to replay it without re-running
// the backtest (spec §6.5).
type CompletedTrial struct {
	Number       int
	OptunaNumber *int
	Sharpe       float64
	Calmar       float64
}

// CompletedTrials returns every COMPLETE trial of a study keyed by artifact
// number (ascending), for resume replay (§6.5). FAIL/RUNNING trials are excluded
// (they are re-run). Used by the coordinator to skip re-evaluating finished work
// while restoring the exact NSGA-II population trajectory from the stored
// objective values.
func (s *Store) CompletedTrials(ctx context.Context, studyTS string) (map[int]CompletedTrial, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT number, optuna_number, sharpe, calmar
		FROM tms.hyperopt_trials
		WHERE study_ts = $1 AND state = 'COMPLETE' AND sharpe IS NOT NULL AND calmar IS NOT NULL
		ORDER BY number ASC`, studyTS)
	if err != nil {
		return nil, fmt.Errorf("hyperopt: load completed trials %s: %w", studyTS, err)
	}
	defer rows.Close()
	out := map[int]CompletedTrial{}
	for rows.Next() {
		var ct CompletedTrial
		var sh, cl *float64
		if err := rows.Scan(&ct.Number, &ct.OptunaNumber, &sh, &cl); err != nil {
			return nil, fmt.Errorf("hyperopt: scan completed trial: %w", err)
		}
		if sh == nil || cl == nil {
			continue
		}
		ct.Sharpe, ct.Calmar = *sh, *cl
		out[ct.Number] = ct
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// scan + marshal helpers
// ---------------------------------------------------------------------------

type scannable interface {
	Scan(dest ...any) error
}

func scanStudyRow(row scannable) (StudyRow, error) {
	var (
		r     StudyRow
		wfEn  bool
		wfFld int
		wfEmb int
		cbRaw []byte
	)
	err := row.Scan(
		&r.TS, &r.StudyName, &r.Strategy, &r.Start, &r.End,
		&r.Directions, &r.Objectives, &r.Seed, &r.NTrials, &r.Workers,
		&wfEn, &wfFld, &wfEmb, &r.CreatedAt, &r.UpdatedAt,
		&r.Status, &r.CompletedTrials, &r.FailedTrials, &r.RunningTrials,
		&r.StartedAt, &r.LastHeartbeatAt, &r.CoordinatorPID, &cbRaw, &r.LastError,
	)
	if err != nil {
		return r, err
	}
	r.Version = 1
	r.WalkForward = WalkForward{Enabled: wfEn, Folds: wfFld, EmbargoDays: wfEmb}
	if len(cbRaw) > 0 && string(cbRaw) != "null" {
		var cb CurrentBest
		if err := json.Unmarshal(cbRaw, &cb); err == nil {
			r.CurrentBest = &cb
		}
	}
	return r, nil
}

// currentBestJSON marshals a CurrentBest (or nil) to JSONB bytes / nil.
func currentBestJSON(cb *CurrentBest) ([]byte, error) {
	if cb == nil {
		return nil, nil
	}
	b, err := json.Marshal(cb)
	if err != nil {
		return nil, fmt.Errorf("hyperopt: marshal current_best: %w", err)
	}
	return b, nil
}

// foldsToJSON renders the per-fold payloads as a JSON array of {"fold": i, ...}
// (the same shape as the artifact folds[]), reusing the pyjson metric ordering.
func foldsToJSON(folds []FoldMetric) ([]byte, error) {
	arr := runs.NewArr()
	for _, f := range folds {
		arr.Append(foldObj(f))
	}
	if len(folds) == 0 {
		return []byte("[]"), nil
	}
	return runs.Marshal(arr), nil
}

// nonNilMap returns m or an empty map (so JSONB is "{}" not "null").
func nonNilMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}
