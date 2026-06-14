//go:build integration

package study_test

// store_pg_test.go exercises the hyperopt DB store + promoter against a REAL
// PostgreSQL (the UPSERT semantics, JSONB columns, param_sets/active_params FK
// and audit trail cannot be faked meaningfully). The ephemeral-container
// bootstrap lives in internal/testutil/pgtest (shared with runner/jobs/runs/api),
// skipped when docker is unavailable (TMS_TEST_NO_DOCKER=1). Migrations apply the
// full schema.

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/byjackchen/trade-tms-go/internal/hyperopt/study"
	"github.com/byjackchen/trade-tms-go/internal/testutil/pgtest"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m, "hyperopt")) }

func requirePG(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return pgtest.RequirePG(t)
}

// truncate clears the hyperopt + strategy tables between tests.
func truncate(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`TRUNCATE tms.hyperopt_trials, tms.hyperopt_studies, tms.active_params, tms.param_sets RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

func seedStudy(t *testing.T, store *study.Store, ts, strategy string) {
	t.Helper()
	now := time.Now().UTC()
	cfg := study.StudyConfig{
		Version:     1,
		StudyName:   "hyperopt-" + strategy + "-" + ts,
		Strategy:    strategy,
		Start:       "2023-01-02",
		End:         "2023-12-29",
		Directions:  []string{"maximize", "maximize"},
		Objectives:  []string{"sharpe", "calmar"},
		Seed:        42,
		NTrials:     4,
		Workers:     1,
		WalkForward: study.WalkForward{Enabled: true, Folds: 2, EmbargoDays: 5},
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	prog := study.Progress{Status: study.StatusRunning, TotalTrials: 4, Workers: 1, StartedAt: &now, UpdatedAt: &now}
	if err := store.UpsertStudy(context.Background(), cfg, prog); err != nil {
		t.Fatalf("UpsertStudy: %v", err)
	}
}

func TestStorePersistAndRead(t *testing.T) {
	pool := requirePG(t)
	truncate(t, pool)
	store := study.NewStore(pool)
	ts := "2026-01-02_03-04-05"
	seedStudy(t, store, ts, "pairs")

	now := time.Now().UTC()
	fin := now.Add(time.Second)
	optuna := 0
	trial := study.TrialArtifact{
		Number:       0,
		OptunaNumber: &optuna,
		Strategy:     "pairs",
		Params:       map[string]any{"lookback": 60.0, "entry_z": 2.0, "exit_z": 0.5, "capital_per_pair_pct": 0.3},
		State:        study.TrialComplete,
		StartedAt:    now,
		FinishedAt:   &fin,
		DurationS:    1.0,
	}
	trial.Metrics.Sharpe = 1.5
	trial.Metrics.Calmar = 2.0
	trial.Metrics.FinalBalanceUSD = 110000
	if err := store.UpsertTrial(context.Background(), ts, trial); err != nil {
		t.Fatalf("UpsertTrial: %v", err)
	}
	// Idempotent re-upsert.
	if err := store.UpsertTrial(context.Background(), ts, trial); err != nil {
		t.Fatalf("UpsertTrial (2nd): %v", err)
	}

	rows, err := store.Trials(context.Background(), ts)
	if err != nil {
		t.Fatalf("Trials: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("trials: got %d want 1 (idempotent)", len(rows))
	}
	if rows[0].Sharpe == nil || *rows[0].Sharpe != 1.5 {
		t.Fatalf("sharpe denorm: %v", rows[0].Sharpe)
	}

	got, err := store.Get(context.Background(), ts)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Strategy != "pairs" || got.NTrials != 4 {
		t.Fatalf("study round-trip mismatch: %+v", got.StudyConfig)
	}

	list, err := store.List(context.Background(), "", 50)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].TS != ts {
		t.Fatalf("list mismatch: %+v", list)
	}

	if _, err := store.Get(context.Background(), "1999-01-01_00-00-00"); err != study.ErrStudyNotFound {
		t.Fatalf("expected ErrStudyNotFound, got %v", err)
	}
}

func TestPromoteWritesActiveParamsWithAudit(t *testing.T) {
	pool := requirePG(t)
	truncate(t, pool)
	store := study.NewStore(pool)
	ts := "2026-02-03_04-05-06"
	seedStudy(t, store, ts, "pairs")

	now := time.Now().UTC()
	optuna := 7
	trial := study.TrialArtifact{
		Number:       3,
		OptunaNumber: &optuna,
		Strategy:     "pairs",
		Params:       map[string]any{"lookback": 80.0, "entry_z": 2.5, "exit_z": 0.4, "capital_per_pair_pct": 0.25},
		State:        study.TrialComplete,
		StartedAt:    now,
		FinishedAt:   &now,
	}
	trial.Metrics.Sharpe = 1.9
	trial.Metrics.Calmar = 2.4
	if err := store.UpsertTrial(context.Background(), ts, trial); err != nil {
		t.Fatal(err)
	}

	promoter := study.NewPromoter(pool)
	promoted, err := promoter.Promote(context.Background(), study.PromoteInput{
		StudyTS: ts, TrialNumber: 3, PromotedBy: "tester", Now: now,
	})
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if len(promoted) != 1 || promoted[0].Strategy != "pairs" {
		t.Fatalf("promoted: %+v", promoted)
	}

	// active_params row carries the audit.
	var (
		sourceID, promotedBy, sourceStudy string
		sourceTrial                       int64
		paramSetID                        int64
	)
	err = pool.QueryRow(context.Background(),
		`SELECT param_set_id, source_id, promoted_by, source_study, source_trial
		 FROM tms.active_params WHERE strategy = 'pairs'`).
		Scan(&paramSetID, &sourceID, &promotedBy, &sourceStudy, &sourceTrial)
	if err != nil {
		t.Fatalf("read active_params: %v", err)
	}
	if sourceID != "hyperopt:"+ts {
		t.Fatalf("source_id: got %q", sourceID)
	}
	if promotedBy != "tester" {
		t.Fatalf("promoted_by: got %q", promotedBy)
	}
	if sourceStudy != ts {
		t.Fatalf("source_study: got %q", sourceStudy)
	}
	if sourceTrial != 7 { // optuna number, not artifact number
		t.Fatalf("source_trial: got %d want 7 (optuna)", sourceTrial)
	}

	// param_set is tuned with provenance metadata.
	var source, tunedStudy string
	var payload []byte
	err = pool.QueryRow(context.Background(),
		`SELECT source, tuned_from_study, payload FROM tms.param_sets WHERE id = $1`, paramSetID).
		Scan(&source, &tunedStudy, &payload)
	if err != nil {
		t.Fatalf("read param_set: %v", err)
	}
	if source != "tuned" || tunedStudy != ts {
		t.Fatalf("param_set provenance: source=%q tuned_from_study=%q", source, tunedStudy)
	}

	// Idempotent: promoting the same trial again reuses the param_set.
	promoted2, err := promoter.Promote(context.Background(), study.PromoteInput{
		StudyTS: ts, TrialNumber: 3, PromotedBy: "tester2", Now: now,
	})
	if err != nil {
		t.Fatalf("Promote (2nd): %v", err)
	}
	if promoted2[0].ParamSetID != paramSetID {
		t.Fatalf("re-promote created a new param_set: %d vs %d", promoted2[0].ParamSetID, paramSetID)
	}
	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM tms.param_sets WHERE strategy = 'pairs'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("param_sets count after re-promote: got %d want 1 (idempotent)", count)
	}
}

func TestPromoteRejectsIncompleteTrial(t *testing.T) {
	pool := requirePG(t)
	truncate(t, pool)
	store := study.NewStore(pool)
	ts := "2026-03-04_05-06-07"
	seedStudy(t, store, ts, "pairs")

	now := time.Now().UTC()
	msg := "boom"
	failTrial := study.TrialArtifact{
		Number:    1,
		Strategy:  "pairs",
		Params:    map[string]any{"lookback": 60.0},
		State:     study.TrialFail,
		Error:     &msg,
		StartedAt: now,
	}
	if err := store.UpsertTrial(context.Background(), ts, failTrial); err != nil {
		t.Fatal(err)
	}
	promoter := study.NewPromoter(pool)
	_, err := promoter.Promote(context.Background(), study.PromoteInput{StudyTS: ts, TrialNumber: 1, PromotedBy: "x"})
	if err == nil {
		t.Fatal("expected error promoting a FAIL trial")
	}
}

// TestPromoteRejectsOutOfBoundsParams: a COMPLETE trial whose recorded params are
// outside the search range MUST NOT promote (finding 5 / adversarial mustFix).
// A malformed / manually-inserted row trips the bounds gate; nothing reaches
// active_params.
func TestPromoteRejectsOutOfBoundsParams(t *testing.T) {
	pool := requirePG(t)
	truncate(t, pool)
	store := study.NewStore(pool)
	ts := "2026-04-05_06-07-08"
	seedStudy(t, store, ts, "pairs")

	now := time.Now().UTC()
	// entry_z search range is [1.5, 3.0]; 99 is wildly out of bounds.
	bad := study.TrialArtifact{
		Number:    2,
		Strategy:  "pairs",
		Params:    map[string]any{"lookback": 60.0, "entry_z": 99.0, "exit_z": 0.5},
		State:     study.TrialComplete,
		StartedAt: now,
	}
	bad.Metrics.Sharpe = 2.0
	bad.Metrics.Calmar = 2.0
	if err := store.UpsertTrial(context.Background(), ts, bad); err != nil {
		t.Fatal(err)
	}
	promoter := study.NewPromoter(pool)
	_, err := promoter.Promote(context.Background(), study.PromoteInput{StudyTS: ts, TrialNumber: 2, PromotedBy: "x"})
	if err == nil {
		t.Fatal("expected ErrInvalidParams promoting an out-of-bounds trial")
	}
	if !errors.Is(err, study.ErrInvalidParams) {
		t.Fatalf("error must wrap ErrInvalidParams, got %v", err)
	}
	// Nothing was written to active_params / param_sets.
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM tms.active_params WHERE strategy = 'pairs'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("a rejected promotion must not write active_params, got %d rows", n)
	}
}

// TestTrialTimeoutSecPersisted: a study config carrying TrialTimeoutSec persists
// to the hyperopt_studies.trial_timeout_sec column (finding 3).
func TestTrialTimeoutSecPersisted(t *testing.T) {
	pool := requirePG(t)
	truncate(t, pool)
	store := study.NewStore(pool)
	ts := "2026-05-06_07-08-09"

	now := time.Now().UTC()
	timeout := 600
	cfg := study.StudyConfig{
		Version: 1, StudyName: "hyperopt-pairs-" + ts, Strategy: "pairs",
		Start: "2023-01-01", End: "2023-12-31",
		Directions: []string{"maximize", "maximize"}, Objectives: []string{"sharpe", "calmar"},
		Seed: 42, NTrials: 4, Workers: 1,
		WalkForward: study.WalkForward{Enabled: true, Folds: 2, EmbargoDays: 5},
		CreatedAt:   now, UpdatedAt: now,
		TrialTimeoutSec: &timeout,
	}
	prog := study.Progress{Status: study.StatusRunning, TotalTrials: 4, Workers: 1, StartedAt: &now, UpdatedAt: &now}
	if err := store.UpsertStudy(context.Background(), cfg, prog); err != nil {
		t.Fatalf("UpsertStudy: %v", err)
	}
	var got *int
	if err := pool.QueryRow(context.Background(),
		`SELECT trial_timeout_sec FROM tms.hyperopt_studies WHERE study_ts = $1`, ts).Scan(&got); err != nil {
		t.Fatalf("read trial_timeout_sec: %v", err)
	}
	if got == nil || *got != 600 {
		t.Fatalf("trial_timeout_sec persisted = %v, want 600", got)
	}
}

// TestStalenessOverrideZombieStudy: a RUNNING study with a stale heartbeat and no
// live coordinator PID is PRESENTED as INTERRUPTED by Get/List (§9.2), without
// mutating the stored row.
func TestStalenessOverrideZombieStudy(t *testing.T) {
	pool := requirePG(t)
	truncate(t, pool)
	store := study.NewStore(pool)
	ts := "2026-06-07_08-09-10"
	seedStudy(t, store, ts, "pairs")

	// Force a stale heartbeat far in the past and a dead PID directly in the row.
	stale := time.Now().UTC().Add(-10 * time.Minute)
	if _, err := pool.Exec(context.Background(),
		`UPDATE tms.hyperopt_studies SET status='RUNNING', last_heartbeat_at=$2, started_at=$2, coordinator_pid=2147483646 WHERE study_ts=$1`,
		ts, stale); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(context.Background(), ts)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != study.StatusInterrupted {
		t.Fatalf("stale zombie study must present INTERRUPTED, got %s", got.Status)
	}
	// The stored row is unchanged (still RUNNING on disk/DB).
	var stored string
	if err := pool.QueryRow(context.Background(),
		`SELECT status FROM tms.hyperopt_studies WHERE study_ts=$1`, ts).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != "RUNNING" {
		t.Fatalf("staleness override must NOT mutate the DB row, got stored status %s", stored)
	}
}
