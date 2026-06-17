package api

// handlers_models.go serves the Model control plane — named, persistable
// portfolio blueprints (which strategies, each weight + param ref + on/off, a
// cash reserve, composite risk; docs/concept-alignment.md §0, §3.3):
//
//	GET    /api/v1/models               list every Model (with members)
//	GET    /api/v1/models/{id}          one Model
//	POST   /api/v1/models               create a Model (audited)
//	PUT    /api/v1/models/{id}          replace a Model (audited)
//	DELETE /api/v1/models/{id}          delete a Model (audited)
//	POST   /api/v1/models/{id}/optimize enqueue a JOINT hyperopt study for the
//	                                    Model (the Models-module "Optimize")
//
// The persistence seam is ModelStore (PG-backed, *model.Store). The mutating
// routes append a tms.audit_log row via AuditWriter (best-effort: an audit-write
// failure is logged but does not fail the mutation, which has already committed).
// model_id resolution for backtests lives in handlers_backtests.go; the optimize
// route shares the hyperopt enqueue path (strategy=joint + model_id).

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/byjackchen/trade-tms-go/internal/jobs"
	"github.com/byjackchen/trade-tms-go/internal/jobs/handlers"
	"github.com/byjackchen/trade-tms-go/internal/model"
)

// ---------------------------------------------------------------------------
// GET /api/v1/models
// ---------------------------------------------------------------------------

func (s *Server) handleModelList(w http.ResponseWriter, r *http.Request) {
	if s.models == nil {
		writeError(w, http.StatusServiceUnavailable, CodeInternal, "model store not configured")
		return
	}
	list, err := s.models.List(r.Context())
	if err != nil {
		internalError(w, s.log, "model list", err)
		return
	}
	if list == nil {
		list = []model.Model{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": list})
}

// ---------------------------------------------------------------------------
// GET /api/v1/models/{id}
// ---------------------------------------------------------------------------

func (s *Server) handleModelGet(w http.ResponseWriter, r *http.Request) {
	if s.models == nil {
		writeError(w, http.StatusServiceUnavailable, CodeInternal, "model store not configured")
		return
	}
	id := strings.TrimSpace(chi.URLParam(r, "id"))
	m, err := s.models.Get(r.Context(), id)
	if errors.Is(err, model.ErrNotFound) {
		writeError(w, http.StatusNotFound, CodeNotFound, fmt.Sprintf("model %q not found", id))
		return
	}
	if err != nil {
		internalError(w, s.log, "model get", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"model": m})
}

// ---------------------------------------------------------------------------
// POST /api/v1/models
// ---------------------------------------------------------------------------

// modelRequest is the create/update body: the Model blueprint plus the audit
// actor. It mirrors model.Model's JSON shape; Validate runs in the Store before
// any DB write, so a malformed Model is rejected as a 422.
type modelRequest struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	CashPct     float64        `json:"cash_pct"`
	Risk        model.Risk     `json:"risk"`
	Members     []model.Member `json:"members"`
	Version     int            `json:"version"`

	Actor string `json:"actor"`
}

// toModel projects the request to a model.Model (the wire-to-domain mapping;
// Validate happens downstream in the Store).
func (req modelRequest) toModel() model.Model {
	return model.Model{
		ID:          strings.TrimSpace(req.ID),
		Name:        req.Name,
		Description: req.Description,
		CashPct:     req.CashPct,
		Risk:        req.Risk,
		Members:     req.Members,
		Version:     req.Version,
	}
}

func (s *Server) handleModelCreate(w http.ResponseWriter, r *http.Request) {
	if s.models == nil {
		writeError(w, http.StatusServiceUnavailable, CodeInternal, "model store not configured")
		return
	}
	var req modelRequest
	if err := decodeStrictJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeValidation, err.Error())
		return
	}
	m := req.toModel()
	if m.ID == "" {
		writeError(w, http.StatusBadRequest, CodeValidation, `"id" is required`)
		return
	}
	if err := m.Validate(); err != nil {
		writeError(w, http.StatusUnprocessableEntity, CodeValidation, err.Error())
		return
	}
	// Reject a duplicate up front with a clean 409 (the bare INSERT would 500 on
	// the PK conflict).
	if _, err := s.models.Get(r.Context(), m.ID); err == nil {
		writeError(w, http.StatusConflict, CodeValidation, fmt.Sprintf("model %q already exists", m.ID))
		return
	} else if !errors.Is(err, model.ErrNotFound) {
		internalError(w, s.log, "model create exists-check", err)
		return
	}
	if err := s.models.Create(r.Context(), m); err != nil {
		internalError(w, s.log, "model create", err)
		return
	}
	s.auditModel(r, actorOrDefault(req.Actor), "model.create", m.ID, map[string]any{
		"name": m.Name, "members": len(m.Members),
	})
	s.log.Info().Str("model_id", m.ID).Str("actor", actorOrDefault(req.Actor)).Msg("model created")
	writeJSON(w, http.StatusCreated, map[string]any{"model": m})
}

// ---------------------------------------------------------------------------
// PUT /api/v1/models/{id}
// ---------------------------------------------------------------------------

func (s *Server) handleModelUpdate(w http.ResponseWriter, r *http.Request) {
	if s.models == nil {
		writeError(w, http.StatusServiceUnavailable, CodeInternal, "model store not configured")
		return
	}
	id := strings.TrimSpace(chi.URLParam(r, "id"))
	var req modelRequest
	if err := decodeStrictJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeValidation, err.Error())
		return
	}
	m := req.toModel()
	// The path id is authoritative; a body id, when present, must agree with it
	// (no silent rename).
	if m.ID == "" {
		m.ID = id
	} else if m.ID != id {
		writeError(w, http.StatusBadRequest, CodeValidation,
			fmt.Sprintf("body id %q does not match path id %q", m.ID, id))
		return
	}
	if err := m.Validate(); err != nil {
		writeError(w, http.StatusUnprocessableEntity, CodeValidation, err.Error())
		return
	}
	if err := s.models.Update(r.Context(), m); errors.Is(err, model.ErrNotFound) {
		writeError(w, http.StatusNotFound, CodeNotFound, fmt.Sprintf("model %q not found", id))
		return
	} else if err != nil {
		internalError(w, s.log, "model update", err)
		return
	}
	s.auditModel(r, actorOrDefault(req.Actor), "model.update", m.ID, map[string]any{
		"name": m.Name, "members": len(m.Members),
	})
	s.log.Info().Str("model_id", m.ID).Str("actor", actorOrDefault(req.Actor)).Msg("model updated")
	writeJSON(w, http.StatusOK, map[string]any{"model": m})
}

// ---------------------------------------------------------------------------
// DELETE /api/v1/models/{id}
// ---------------------------------------------------------------------------

func (s *Server) handleModelDelete(w http.ResponseWriter, r *http.Request) {
	if s.models == nil {
		writeError(w, http.StatusServiceUnavailable, CodeInternal, "model store not configured")
		return
	}
	id := strings.TrimSpace(chi.URLParam(r, "id"))
	actor := actorOrDefault(actorFromQuery(r))
	if err := s.models.Delete(r.Context(), id); errors.Is(err, model.ErrNotFound) {
		writeError(w, http.StatusNotFound, CodeNotFound, fmt.Sprintf("model %q not found", id))
		return
	} else if err != nil {
		internalError(w, s.log, "model delete", err)
		return
	}
	s.auditModel(r, actor, "model.delete", id, nil)
	s.log.Info().Str("model_id", id).Str("actor", actor).Msg("model deleted")
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

// ---------------------------------------------------------------------------
// POST /api/v1/models/{id}/optimize  — joint (multi-strategy) tuning
// ---------------------------------------------------------------------------

// modelOptimizeRequest is the enqueue body for a Model's joint optimisation: the
// hyperopt search window + GA knobs, minus the strategy selector (the Model's
// members define the joint search). It mirrors hyperoptRequest's tunables.
type modelOptimizeRequest struct {
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

func (s *Server) handleModelOptimize(w http.ResponseWriter, r *http.Request) {
	if s.models == nil {
		writeError(w, http.StatusServiceUnavailable, CodeInternal, "model store not configured")
		return
	}
	id := strings.TrimSpace(chi.URLParam(r, "id"))
	mdl, err := s.models.Get(r.Context(), id)
	if errors.Is(err, model.ErrNotFound) {
		writeError(w, http.StatusNotFound, CodeNotFound, fmt.Sprintf("model %q not found", id))
		return
	}
	if err != nil {
		internalError(w, s.log, "model optimize resolve", err)
		return
	}

	var req modelOptimizeRequest
	if err := decodeStrictJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeValidation, err.Error())
		return
	}
	if strings.TrimSpace(req.Start) == "" || strings.TrimSpace(req.End) == "" {
		writeError(w, http.StatusBadRequest, CodeValidation, `"start" and "end" are required (YYYY-MM-DD)`)
		return
	}
	// A joint study tuning a SEPA member needs a stock universe to search over.
	if hasMember(mdl, model.StrategySEPA) && len(req.Tickers) == 0 && req.Universe == nil {
		writeError(w, http.StatusBadRequest, CodeValidation,
			`this Model has a SEPA member; supply a stock universe ("tickers" or "universe")`)
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

	// strategy=joint is the multi-strategy search the optimizer runs; model_id
	// records the Model the study targets and model carries the resolved blueprint
	// the worker drops in (its ACTIVE members + weights + risk drive assembly and
	// the universe, mirroring the backtest path — docs/concept-alignment.md §3.3).
	payload := map[string]any{
		"strategy": "joint",
		"model_id": mdl.ID,
		"model":    mdl,
		"start":    req.Start,
		"end":      req.End,
	}
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
		internalError(w, s.log, "enqueue model optimize", err)
		return
	}
	s.log.Info().Int64("job_id", job.ID).Str("model_id", mdl.ID).Bool("deduped", deduped).
		Str("actor", actorOrDefault(req.Actor)).Msg("model optimize (joint hyperopt) enqueued")
	writeJSON(w, http.StatusAccepted, map[string]any{"job": jobToJSON(job), "deduped": deduped})
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// hasMember reports whether the Model has a member with the given strategy id.
func hasMember(m *model.Model, strategyID string) bool {
	for _, mem := range m.Members {
		if mem.StrategyID == strategyID {
			return true
		}
	}
	return false
}

// auditModel appends one tms.audit_log row for a Model mutation (best-effort: a
// write failure is logged, not surfaced — the mutation already committed).
func (s *Server) auditModel(r *http.Request, actor, action, modelID string, details map[string]any) {
	if s.auditLog == nil {
		return
	}
	if err := s.auditLog.WriteAudit(r.Context(), AuditRecord{
		Actor: actor, Action: action, Entity: "model", EntityID: modelID, Details: details,
	}); err != nil {
		s.log.Warn().Err(err).Str("action", action).Str("model_id", modelID).
			Msg("model audit write failed; mutation already committed")
	}
}

// actorFromQuery reads the optional ?actor= for DELETE (which carries no JSON
// body to hold an actor field).
func actorFromQuery(r *http.Request) string {
	return strings.TrimSpace(r.URL.Query().Get("actor"))
}
