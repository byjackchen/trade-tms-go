package api

// handlers_hyperopt_test.go is the contract test for the hyperopt control plane
// (POST/GET /api/v1/hyperopt*, promote). It drives in-memory stubs through the
// wired router so every endpoint's status code + envelope shape is asserted
// without a database.

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/hyperopt/study"
	"github.com/byjackchen/trade-tms-go/internal/jobs/handlers"
)

// ---------------------------------------------------------------------------
// stubs
// ---------------------------------------------------------------------------

type stubHyperoptReader struct {
	list      []study.StudyRow
	get       *study.StudyRow
	trials    []study.TrialRow
	notFound  bool
	err       error
	lastStrat string
}

func (s *stubHyperoptReader) List(_ context.Context, strategy string, _ int) ([]study.StudyRow, error) {
	s.lastStrat = strategy
	return s.list, s.err
}

func (s *stubHyperoptReader) Get(_ context.Context, _ string) (*study.StudyRow, error) {
	if s.notFound {
		return nil, study.ErrStudyNotFound
	}
	if s.err != nil {
		return nil, s.err
	}
	return s.get, nil
}

func (s *stubHyperoptReader) Trials(_ context.Context, _ string) ([]study.TrialRow, error) {
	if s.notFound {
		return nil, study.ErrStudyNotFound
	}
	return s.trials, s.err
}

type stubPromoter struct {
	out      []study.PromotedStrategy
	err      error
	lastIn   study.PromoteInput
	notFound bool

	compOut   *study.PromotedComposition
	compErr   error
	lastCompIn study.PromoteCompositionInput
}

func (s *stubPromoter) Promote(_ context.Context, in study.PromoteInput) ([]study.PromotedStrategy, error) {
	s.lastIn = in
	if s.notFound {
		return nil, study.ErrStudyNotFound
	}
	if s.err != nil {
		return nil, s.err
	}
	return s.out, nil
}

func (s *stubPromoter) PromoteComposition(_ context.Context, in study.PromoteCompositionInput) (*study.PromotedComposition, error) {
	s.lastCompIn = in
	if s.notFound {
		return nil, study.ErrStudyNotFound
	}
	if s.compErr != nil {
		return nil, s.compErr
	}
	return s.compOut, nil
}

func sampleStudyRow(ts string) study.StudyRow {
	now := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	r := study.StudyRow{
		TS:              ts,
		Status:          study.StatusComplete,
		CompletedTrials: 8,
		FailedTrials:    0,
		RunningTrials:   0,
		StartedAt:       &now,
		LastHeartbeatAt: &now,
		CurrentBest:     &study.CurrentBest{Trial: 3, Sharpe: 1.9, Calmar: 2.4},
	}
	r.Version = 1
	r.StudyName = "hyperopt-pairs-" + ts
	r.Strategy = "pairs"
	r.Start = "2023-01-02"
	r.End = "2023-12-29"
	r.Directions = []string{"maximize", "maximize"}
	r.Objectives = []string{"sharpe", "calmar"}
	r.Seed = 42
	r.NTrials = 8
	r.Workers = 1
	r.WalkForward = study.WalkForward{Enabled: true, Folds: 2, EmbargoDays: 5}
	r.CreatedAt = now
	r.UpdatedAt = now
	return r
}

// ---------------------------------------------------------------------------
// POST /api/v1/hyperopt
// ---------------------------------------------------------------------------

func TestHyperoptEnqueue(t *testing.T) {
	ts := newTestServer(t)
	body := `{"strategy":"pairs","start":"2023-01-02","end":"2023-12-29","population":4,"generations":2}`
	rec := ts.do(t, http.MethodPost, "/api/v1/hyperopt", strings.NewReader(body), true)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	require.Len(t, ts.jobs.enqueued, 1)
	require.Equal(t, handlers.KindHyperoptRun, ts.jobs.enqueued[0].Kind)
}

func TestHyperoptEnqueueRejectsUnknownStrategy(t *testing.T) {
	ts := newTestServer(t)
	rec := ts.do(t, http.MethodPost, "/api/v1/hyperopt",
		strings.NewReader(`{"strategy":"bogus","start":"2023-01-02","end":"2023-12-29"}`), true)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, CodeValidation, errCode(t, rec))
}

func TestHyperoptEnqueueSEPARequiresUniverse(t *testing.T) {
	ts := newTestServer(t)
	rec := ts.do(t, http.MethodPost, "/api/v1/hyperopt",
		strings.NewReader(`{"strategy":"sepa","start":"2023-01-02","end":"2023-12-29"}`), true)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

// ---------------------------------------------------------------------------
// GET /api/v1/hyperopt
// ---------------------------------------------------------------------------

func TestHyperoptList(t *testing.T) {
	ts := newTestServer(t)
	ts.hyperopt.list = []study.StudyRow{sampleStudyRow("2026-06-13_12-00-00")}
	rec := ts.do(t, http.MethodGet, "/api/v1/hyperopt", nil, true)
	require.Equal(t, http.StatusOK, rec.Code)
	m := decodeBody(t, rec)
	studies, ok := m["studies"].([]any)
	require.True(t, ok)
	require.Len(t, studies, 1)
	first := studies[0].(map[string]any)
	require.Equal(t, "2026-06-13_12-00-00", first["ts"])
	require.Contains(t, first, "config")
	require.Contains(t, first, "progress")
}

func TestHyperoptListStrategyFilter(t *testing.T) {
	ts := newTestServer(t)
	rec := ts.do(t, http.MethodGet, "/api/v1/hyperopt?strategy=pairs", nil, true)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "pairs", ts.hyperopt.lastStrat)
}

// ---------------------------------------------------------------------------
// GET /api/v1/hyperopt/{id}
// ---------------------------------------------------------------------------

func TestHyperoptGet(t *testing.T) {
	ts := newTestServer(t)
	row := sampleStudyRow("2026-06-13_12-00-00")
	ts.hyperopt.get = &row
	rec := ts.do(t, http.MethodGet, "/api/v1/hyperopt/2026-06-13_12-00-00", nil, true)
	require.Equal(t, http.StatusOK, rec.Code)
	m := decodeBody(t, rec)
	st := m["study"].(map[string]any)
	cfg := st["config"].(map[string]any)
	require.Equal(t, "pairs", cfg["strategy"])
	prog := st["progress"].(map[string]any)
	require.Equal(t, "COMPLETE", prog["status"])
	best := prog["current_best"].(map[string]any)
	require.EqualValues(t, 3, best["trial"])
}

func TestHyperoptGetInvalidID(t *testing.T) {
	ts := newTestServer(t)
	rec := ts.do(t, http.MethodGet, "/api/v1/hyperopt/not-a-ts", nil, true)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, CodeValidation, errCode(t, rec))
}

func TestHyperoptGetNotFound(t *testing.T) {
	ts := newTestServer(t)
	ts.hyperopt.notFound = true
	rec := ts.do(t, http.MethodGet, "/api/v1/hyperopt/2026-06-13_12-00-00", nil, true)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Equal(t, CodeNotFound, errCode(t, rec))
}

// ---------------------------------------------------------------------------
// GET /api/v1/hyperopt/{id}/trials  (pareto-front flag + per-fold breakdown)
// ---------------------------------------------------------------------------

func TestHyperoptTrialsParetoFlag(t *testing.T) {
	ts := newTestServer(t)
	s1, c1 := 1.0, 1.0
	s2, c2 := 2.0, 0.5
	s3, c3 := 0.5, 0.5 // dominated by trial 0 (1.0,1.0)
	ts.hyperopt.trials = []study.TrialRow{
		{Number: 0, State: study.TrialComplete, Sharpe: &s1, Calmar: &c1, Folds: []byte(`[{"fold":0,"sharpe":1.0}]`)},
		{Number: 1, State: study.TrialComplete, Sharpe: &s2, Calmar: &c2},
		{Number: 2, State: study.TrialComplete, Sharpe: &s3, Calmar: &c3},
	}
	rec := ts.do(t, http.MethodGet, "/api/v1/hyperopt/2026-06-13_12-00-00/trials", nil, true)
	require.Equal(t, http.StatusOK, rec.Code)
	m := decodeBody(t, rec)
	trials := m["trials"].([]any)
	require.Len(t, trials, 3)
	flag := func(i int) bool { return trials[i].(map[string]any)["pareto_front"].(bool) }
	require.True(t, flag(0), "trial 0 (1,1) on front")
	require.True(t, flag(1), "trial 1 (2,0.5) on front")
	require.False(t, flag(2), "trial 2 (0.5,0.5) dominated")
	// per-fold breakdown surfaced.
	require.Contains(t, trials[0].(map[string]any), "folds")
}

func TestHyperoptTrialsNotFound(t *testing.T) {
	ts := newTestServer(t)
	ts.hyperopt.notFound = true
	rec := ts.do(t, http.MethodGet, "/api/v1/hyperopt/2026-06-13_12-00-00/trials", nil, true)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

// ---------------------------------------------------------------------------
// POST /api/v1/hyperopt/{id}/promote
// ---------------------------------------------------------------------------

func TestHyperoptPromote(t *testing.T) {
	ts := newTestServer(t)
	ts.promoter.out = []study.PromotedStrategy{{Strategy: "pairs", ParamSetID: 12, Version: 1}}
	rec := ts.do(t, http.MethodPost, "/api/v1/hyperopt/2026-06-13_12-00-00/promote",
		strings.NewReader(`{"trial_id":3,"actor":"alice"}`), true)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	m := decodeBody(t, rec)
	require.EqualValues(t, 3, m["trial_id"])
	require.Equal(t, "2026-06-13_12-00-00", ts.promoter.lastIn.StudyTS)
	require.Equal(t, 3, ts.promoter.lastIn.TrialNumber)
	require.Equal(t, "api:alice", ts.promoter.lastIn.PromotedBy)
}

func TestHyperoptPromoteMissingTrialID(t *testing.T) {
	ts := newTestServer(t)
	rec := ts.do(t, http.MethodPost, "/api/v1/hyperopt/2026-06-13_12-00-00/promote",
		strings.NewReader(`{}`), true)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, CodeValidation, errCode(t, rec))
}

func TestHyperoptPromoteNotPromotable(t *testing.T) {
	ts := newTestServer(t)
	ts.promoter.err = study.ErrTrialNotPromotable
	rec := ts.do(t, http.MethodPost, "/api/v1/hyperopt/2026-06-13_12-00-00/promote",
		strings.NewReader(`{"trial_id":1}`), true)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
}

func TestHyperoptUnauthorized(t *testing.T) {
	ts := newTestServer(t)
	rec := ts.do(t, http.MethodGet, "/api/v1/hyperopt", nil, false)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}
