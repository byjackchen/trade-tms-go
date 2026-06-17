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
//
// A Model COMPOSES already-tuned strategies + weights + risk and is VALIDATED by
// Backtest — it never re-tunes params (per-strategy Hyperopt in the Strategies
// module owns that). There is intentionally NO Model-level optimize endpoint; the
// hyperopt engine's internal "joint" study code stays dormant, never exposed here.
//
// The persistence seam is ModelStore (PG-backed, *model.Store). The mutating
// routes append a tms.audit_log row via AuditWriter (best-effort: an audit-write
// failure is logged but does not fail the mutation, which has already committed).
// model_id resolution for backtests lives in handlers_backtests.go.

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

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
// helpers
// ---------------------------------------------------------------------------

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
