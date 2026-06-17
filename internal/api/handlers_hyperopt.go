package api

// handlers_hyperopt.go serves the hyperopt study control plane:
//
//	POST /api/v1/hyperopt               enqueue a hyperopt.run study job (202 + job)
//	GET  /api/v1/hyperopt               list studies (newest-first)
//	GET  /api/v1/hyperopt/{id}          study detail: config + progress + best
//	GET  /api/v1/hyperopt/{id}/trials   trials (pareto-front flag + per-fold breakdown)
//	POST /api/v1/hyperopt/{id}/promote  promote a chosen trial's params (audited)
//
// {id} is the study_ts (UTC %Y-%m-%d_%H-%M-%S directory name / PK). The DB
// (research.hyperopt_*) is the source of truth (locked decision 3); reads go
// through study.Store, promotion through study.Promoter.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/byjackchen/trade-tms-go/internal/hyperopt/study"
	"github.com/byjackchen/trade-tms-go/internal/jobs"
	"github.com/byjackchen/trade-tms-go/internal/jobs/handlers"
)

const (
	defaultHyperoptLimit = 50
	maxHyperoptLimit     = 500
)

// studyTSPattern is the %Y-%m-%d_%H-%M-%S study_ts shape (also the dir name).
func validStudyTS(ts string) bool {
	_, err := time.Parse("2006-01-02_15-04-05", ts)
	return err == nil
}

// ---------------------------------------------------------------------------
// POST /api/v1/hyperopt
// ---------------------------------------------------------------------------

type hyperoptRequest struct {
	Strategy        string         `json:"strategy"`
	Start           string         `json:"start"`
	End             string         `json:"end"`
	Population      int            `json:"population"`
	Generations     int            `json:"generations"`
	Seed            int64          `json:"seed"`
	Workers         int            `json:"workers"`
	WalkForward     *bool          `json:"walk_forward"`
	Folds           int            `json:"folds"`
	EmbargoDays     int            `json:"embargo_days"`
	Tickers         []string       `json:"tickers"`
	Universe        map[string]any `json:"universe"`
	StartingBalance *float64       `json:"starting_balance"`
	StudyTS         string         `json:"study_ts"`
	TrialTimeoutSec *int           `json:"trial_timeout_sec"`
	Resume          bool           `json:"resume"`

	Actor       string `json:"actor"`
	MaxAttempts int32  `json:"max_attempts"`
	DedupeKey   string `json:"dedupe_key"`
}

func (s *Server) handleHyperoptEnqueue(w http.ResponseWriter, r *http.Request) {
	var req hyperoptRequest
	if err := decodeStrictJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeValidation, err.Error())
		return
	}
	// SINGLE-strategy tuning only (docs/concept-alignment.md §3.3): params are
	// tuned per-strategy here. Joint (multi-strategy) tuning is dropped from the
	// product — Compositions compose already-tuned strategies and are validated by
	// Backtest; the engine's internal joint study code stays dormant, unexposed.
	switch req.Strategy {
	case "sepa", "sector_rotation", "pairs":
	default:
		writeError(w, http.StatusBadRequest, CodeValidation,
			fmt.Sprintf("unsupported strategy %q (want sepa|sector_rotation|pairs)", req.Strategy))
		return
	}
	if strings.TrimSpace(req.Start) == "" || strings.TrimSpace(req.End) == "" {
		writeError(w, http.StatusBadRequest, CodeValidation, `"start" and "end" are required (YYYY-MM-DD)`)
		return
	}
	if req.Strategy == "sepa" && len(req.Tickers) == 0 && req.Universe == nil {
		writeError(w, http.StatusBadRequest, CodeValidation,
			fmt.Sprintf("strategy %q requires a stock universe (\"tickers\" or \"universe\")", req.Strategy))
		return
	}
	if req.Generations < 0 || req.Population < 0 || req.Folds < 0 || req.EmbargoDays < 0 {
		writeError(w, http.StatusBadRequest, CodeValidation, "population/generations/folds/embargo_days must be >= 0")
		return
	}
	if req.MaxAttempts < 0 || req.MaxAttempts > 10 {
		writeError(w, http.StatusBadRequest, CodeValidation, "max_attempts must be in [0, 10] (0 = default 1)")
		return
	}

	payload := map[string]any{"strategy": req.Strategy, "start": req.Start, "end": req.End}
	if req.Population != 0 {
		payload["population"] = req.Population
	}
	if req.Generations != 0 {
		payload["generations"] = req.Generations
	}
	if req.Seed != 0 {
		payload["seed"] = req.Seed
	}
	if req.Workers != 0 {
		payload["workers"] = req.Workers
	}
	if req.WalkForward != nil {
		payload["walk_forward"] = *req.WalkForward
	}
	if req.Folds != 0 {
		payload["folds"] = req.Folds
	}
	if req.EmbargoDays != 0 {
		payload["embargo_days"] = req.EmbargoDays
	}
	if len(req.Tickers) > 0 {
		payload["tickers"] = req.Tickers
	}
	if req.Universe != nil {
		payload["universe"] = req.Universe
	}
	if req.StartingBalance != nil {
		payload["starting_balance"] = *req.StartingBalance
	}
	if req.StudyTS != "" {
		payload["study_ts"] = req.StudyTS
	}
	if req.TrialTimeoutSec != nil {
		if *req.TrialTimeoutSec < 0 {
			writeError(w, http.StatusBadRequest, CodeValidation, "trial_timeout_sec must be >= 0 (0 disables)")
			return
		}
		payload["trial_timeout_sec"] = *req.TrialTimeoutSec
	}
	if req.Resume {
		if req.StudyTS == "" {
			writeError(w, http.StatusBadRequest, CodeValidation, `"resume" requires "study_ts"`)
			return
		}
		payload["resume"] = true
	}

	job, deduped, err := s.jobs.Enqueue(r.Context(), jobs.EnqueueParams{
		Kind:        handlers.KindHyperoptRun,
		Payload:     payload,
		DedupeKey:   strings.TrimSpace(req.DedupeKey),
		MaxAttempts: req.MaxAttempts,
		Actor:       actorOrDefault(req.Actor),
	})
	if err != nil {
		internalError(w, s.log, "enqueue hyperopt.run", err)
		return
	}
	s.log.Info().Int64("job_id", job.ID).Bool("deduped", deduped).
		Str("actor", actorOrDefault(req.Actor)).Msg("hyperopt.run enqueued")
	writeJSON(w, http.StatusAccepted, map[string]any{"job": jobToJSON(job), "deduped": deduped})
}

// ---------------------------------------------------------------------------
// GET /api/v1/hyperopt
// ---------------------------------------------------------------------------

func (s *Server) handleHyperoptList(w http.ResponseWriter, r *http.Request) {
	if s.hyperopt == nil {
		writeError(w, http.StatusServiceUnavailable, CodeInternal, "hyperopt reader not configured")
		return
	}
	limit, ok := parseLimit(w, r, defaultHyperoptLimit, maxHyperoptLimit)
	if !ok {
		return
	}
	strategy := strings.TrimSpace(r.URL.Query().Get("strategy"))
	if strategy != "" {
		switch strategy {
		case "sepa", "sector_rotation", "pairs", "joint", "composition":
		default:
			writeError(w, http.StatusBadRequest, CodeValidation,
				fmt.Sprintf("unknown strategy %q", strategy))
			return
		}
	}
	rows, err := s.hyperopt.List(r.Context(), strategy, limit)
	if err != nil {
		internalError(w, s.log, "hyperopt list", err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, studyRowToJSON(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"studies": out})
}

// ---------------------------------------------------------------------------
// GET /api/v1/hyperopt/{id}
// ---------------------------------------------------------------------------

func (s *Server) handleHyperoptGet(w http.ResponseWriter, r *http.Request) {
	if s.hyperopt == nil {
		writeError(w, http.StatusServiceUnavailable, CodeInternal, "hyperopt reader not configured")
		return
	}
	ts, ok := studyTSParam(w, r)
	if !ok {
		return
	}
	row, err := s.hyperopt.Get(r.Context(), ts)
	if errors.Is(err, study.ErrStudyNotFound) {
		writeError(w, http.StatusNotFound, CodeNotFound, fmt.Sprintf("study %s not found", ts))
		return
	}
	if err != nil {
		internalError(w, s.log, "hyperopt get", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"study": studyRowToJSON(*row)})
}

// ---------------------------------------------------------------------------
// GET /api/v1/hyperopt/{id}/trials
// ---------------------------------------------------------------------------

func (s *Server) handleHyperoptTrials(w http.ResponseWriter, r *http.Request) {
	if s.hyperopt == nil {
		writeError(w, http.StatusServiceUnavailable, CodeInternal, "hyperopt reader not configured")
		return
	}
	ts, ok := studyTSParam(w, r)
	if !ok {
		return
	}
	rows, err := s.hyperopt.Trials(r.Context(), ts)
	if errors.Is(err, study.ErrStudyNotFound) {
		writeError(w, http.StatusNotFound, CodeNotFound, fmt.Sprintf("study %s not found", ts))
		return
	}
	if err != nil {
		internalError(w, s.log, "hyperopt trials", err)
		return
	}
	front := paretoFront(rows)
	out := make([]map[string]any, 0, len(rows))
	for _, t := range rows {
		out = append(out, trialRowToJSON(t, front[t.Number]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"trials": out})
}

// ---------------------------------------------------------------------------
// POST /api/v1/hyperopt/{id}/promote
// ---------------------------------------------------------------------------

type promoteRequest struct {
	TrialID *int   `json:"trial_id"`
	Actor   string `json:"actor"`
}

func (s *Server) handleHyperoptPromote(w http.ResponseWriter, r *http.Request) {
	if s.promoter == nil {
		writeError(w, http.StatusServiceUnavailable, CodeInternal, "hyperopt promoter not configured")
		return
	}
	ts, ok := studyTSParam(w, r)
	if !ok {
		return
	}
	var req promoteRequest
	if err := decodeStrictJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeValidation, err.Error())
		return
	}
	if req.TrialID == nil || *req.TrialID < 0 {
		writeError(w, http.StatusBadRequest, CodeValidation, `"trial_id" is required (non-negative integer)`)
		return
	}
	promoted, err := s.promoter.Promote(r.Context(), study.PromoteInput{
		StudyTS:     ts,
		TrialNumber: *req.TrialID,
		PromotedBy:  actorOrDefault(req.Actor),
	})
	switch {
	case errors.Is(err, study.ErrStudyNotFound):
		writeError(w, http.StatusNotFound, CodeNotFound, fmt.Sprintf("study %s not found", ts))
		return
	case errors.Is(err, study.ErrTrialNotPromotable):
		writeError(w, http.StatusUnprocessableEntity, CodeValidation, err.Error())
		return
	case errors.Is(err, study.ErrInvalidParams):
		writeError(w, http.StatusUnprocessableEntity, CodeValidation, err.Error())
		return
	case err != nil:
		internalError(w, s.log, "hyperopt promote", err)
		return
	}
	out := make([]map[string]any, 0, len(promoted))
	for _, p := range promoted {
		out = append(out, map[string]any{
			"strategy": p.Strategy, "param_set_id": p.ParamSetID, "version": p.Version,
		})
	}
	s.log.Info().Str("study_ts", ts).Int("trial", *req.TrialID).
		Str("actor", actorOrDefault(req.Actor)).Msg("hyperopt trial promoted")
	writeJSON(w, http.StatusOK, map[string]any{
		"study_ts": ts, "trial_id": *req.TrialID, "promoted": out,
	})
}

// ---------------------------------------------------------------------------
// projection helpers
// ---------------------------------------------------------------------------

func studyRowToJSON(r study.StudyRow) map[string]any {
	cfg := map[string]any{
		"version":      r.Version,
		"study_name":   r.StudyName,
		"strategy":     r.Strategy,
		"kind":         string(r.Kind),
		"start":        r.Start,
		"end":          r.End,
		"directions":   r.Directions,
		"objectives":   r.Objectives,
		"seed":         r.Seed,
		"n_trials":     r.NTrials,
		"workers":      r.Workers,
		"walk_forward": map[string]any{"enabled": r.WalkForward.Enabled, "folds": r.WalkForward.Folds, "embargo_days": r.WalkForward.EmbargoDays},
		"created_at":   r.CreatedAt.UTC().Format(time.RFC3339Nano),
		"updated_at":   r.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
	if r.CompositionID != "" {
		cfg["composition_id"] = r.CompositionID
	}
	progress := map[string]any{
		"status":            string(r.Status),
		"completed_trials":  r.CompletedTrials,
		"failed_trials":     r.FailedTrials,
		"running_trials":    r.RunningTrials,
		"total_trials":      r.NTrials,
		"workers":           r.Workers,
		"started_at":        isoPtr(r.StartedAt),
		"last_heartbeat_at": isoPtr(r.LastHeartbeatAt),
		"coordinator_pid":   r.CoordinatorPID,
		"current_best":      currentBestJSON(r.CurrentBest),
		"last_error":        r.LastError,
	}
	return map[string]any{"ts": r.TS, "config": cfg, "progress": progress}
}

func currentBestJSON(cb *study.CurrentBest) any {
	if cb == nil {
		return nil
	}
	return map[string]any{"trial": cb.Trial, "sharpe": cb.Sharpe, "calmar": cb.Calmar}
}

func trialRowToJSON(t study.TrialRow, pareto bool) map[string]any {
	m := map[string]any{
		"number":        t.Number,
		"optuna_number": t.OptunaNumber,
		"strategy":      t.Strategy,
		"params":        rawOrNull(t.Params),
		"metrics":       rawOrNull(t.Metrics),
		"folds":         rawOrEmptyArr(t.Folds),
		"state":         string(t.State),
		"sharpe":        t.Sharpe,
		"calmar":        t.Calmar,
		"started_at":    t.StartedAt.UTC().Format(time.RFC3339Nano),
		"finished_at":   isoPtr(t.FinishedAt),
		"duration_sec":  t.DurationS,
		"run_dump_ts":   t.RunDumpTS,
		"error":         t.Error,
		"pareto_front":  pareto,
	}
	return m
}

// paretoFront returns the set of trial numbers on the Pareto front of the
// COMPLETE trials over (sharpe, calmar), both maximized. A trial is dominated iff
// some other trial has sharpe' >= sharpe and calmar' >= calmar with at least one
// strict improvement (weak dominance; §10 pareto_trials). Duplicates are all kept
// (neither dominates the other).
func paretoFront(rows []study.TrialRow) map[int]bool {
	type pt struct {
		num            int
		sharpe, calmar float64
		ok             bool
	}
	pts := make([]pt, 0, len(rows))
	for _, t := range rows {
		if t.State != study.TrialComplete || t.Sharpe == nil || t.Calmar == nil {
			continue
		}
		pts = append(pts, pt{num: t.Number, sharpe: *t.Sharpe, calmar: *t.Calmar, ok: true})
	}
	front := make(map[int]bool, len(pts))
	for i := range pts {
		dominated := false
		for j := range pts {
			if i == j {
				continue
			}
			a, b := pts[j], pts[i]
			if a.sharpe >= b.sharpe && a.calmar >= b.calmar && (a.sharpe > b.sharpe || a.calmar > b.calmar) {
				dominated = true
				break
			}
		}
		if !dominated {
			front[pts[i].num] = true
		}
	}
	return front
}

// rawOrEmptyArr renders a JSONB array or "[]" when empty/absent.
func rawOrEmptyArr(raw json.RawMessage) any {
	if len(raw) == 0 {
		return jsonRaw([]byte("[]"))
	}
	return jsonRaw(raw)
}

func isoPtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func studyTSParam(w http.ResponseWriter, r *http.Request) (string, bool) {
	ts := chi.URLParam(r, "id")
	if !validStudyTS(ts) {
		writeError(w, http.StatusBadRequest, CodeValidation,
			fmt.Sprintf("invalid study id %q (want UTC %%Y-%%m-%%d_%%H-%%M-%%S)", ts))
		return "", false
	}
	return ts, true
}
