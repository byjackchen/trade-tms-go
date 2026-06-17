package api

// handlers_compositions.go serves the Composition control plane — named,
// persistable portfolio blueprints (which strategies, each weight + param ref +
// on/off, a cash reserve, composite risk; docs/concept-alignment.md §0, §3.3):
//
//	GET    /api/v1/compositions               list every Composition (with members)
//	GET    /api/v1/compositions/{id}          one Composition
//	POST   /api/v1/compositions               create a Composition (audited)
//	PUT    /api/v1/compositions/{id}          replace a Composition (audited)
//	DELETE /api/v1/compositions/{id}          delete a Composition (audited)
//
// A Composition COMPOSES already-tuned strategies + weights + risk and is
// VALIDATED by Backtest — it never re-tunes params (per-strategy Hyperopt in the
// Strategies module owns that). There is intentionally NO Composition-level
// optimize endpoint; the hyperopt engine's internal "joint" study code stays
// dormant, never exposed here.
//
// The persistence seam is CompositionStore (PG-backed, *composition.Store). The
// mutating routes append a tms.audit_log row via AuditWriter (best-effort: an
// audit-write failure is logged but does not fail the mutation, which has already
// committed). composition_id resolution for backtests lives in handlers_backtests.go.

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/byjackchen/trade-tms-go/internal/composition"
)

// ---------------------------------------------------------------------------
// GET /api/v1/compositions
// ---------------------------------------------------------------------------

func (s *Server) handleCompositionList(w http.ResponseWriter, r *http.Request) {
	if s.compositions == nil {
		writeError(w, http.StatusServiceUnavailable, CodeInternal, "composition store not configured")
		return
	}
	list, err := s.compositions.List(r.Context())
	if err != nil {
		internalError(w, s.log, "composition list", err)
		return
	}
	if list == nil {
		list = []composition.Composition{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"compositions": list})
}

// ---------------------------------------------------------------------------
// GET /api/v1/compositions/{id}
// ---------------------------------------------------------------------------

func (s *Server) handleCompositionGet(w http.ResponseWriter, r *http.Request) {
	if s.compositions == nil {
		writeError(w, http.StatusServiceUnavailable, CodeInternal, "composition store not configured")
		return
	}
	id := strings.TrimSpace(chi.URLParam(r, "id"))
	m, err := s.compositions.Get(r.Context(), id)
	if errors.Is(err, composition.ErrNotFound) {
		writeError(w, http.StatusNotFound, CodeNotFound, fmt.Sprintf("composition %q not found", id))
		return
	}
	if err != nil {
		internalError(w, s.log, "composition get", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"composition": m})
}

// ---------------------------------------------------------------------------
// POST /api/v1/compositions
// ---------------------------------------------------------------------------

// compositionRequest is the create/update body: the Composition blueprint plus
// the audit actor. It mirrors composition.Composition's JSON shape; Validate runs
// in the Store before any DB write, so a malformed Composition is rejected as a 422.
type compositionRequest struct {
	ID          string               `json:"id"`
	Name        string               `json:"name"`
	Description string               `json:"description"`
	CashPct     float64              `json:"cash_pct"`
	Risk        composition.Risk     `json:"risk"`
	Members     []composition.Member `json:"members"`
	Version     int                  `json:"version"`

	Actor string `json:"actor"`
}

// toComposition projects the request to a composition.Composition (the
// wire-to-domain mapping; Validate happens downstream in the Store).
func (req compositionRequest) toComposition() composition.Composition {
	return composition.Composition{
		ID:          strings.TrimSpace(req.ID),
		Name:        req.Name,
		Description: req.Description,
		CashPct:     req.CashPct,
		Risk:        req.Risk,
		Members:     req.Members,
		Version:     req.Version,
	}
}

func (s *Server) handleCompositionCreate(w http.ResponseWriter, r *http.Request) {
	if s.compositions == nil {
		writeError(w, http.StatusServiceUnavailable, CodeInternal, "composition store not configured")
		return
	}
	var req compositionRequest
	if err := decodeStrictJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeValidation, err.Error())
		return
	}
	m := req.toComposition()
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
	if _, err := s.compositions.Get(r.Context(), m.ID); err == nil {
		writeError(w, http.StatusConflict, CodeValidation, fmt.Sprintf("composition %q already exists", m.ID))
		return
	} else if !errors.Is(err, composition.ErrNotFound) {
		internalError(w, s.log, "composition create exists-check", err)
		return
	}
	if err := s.compositions.Create(r.Context(), m); err != nil {
		internalError(w, s.log, "composition create", err)
		return
	}
	s.auditComposition(r, actorOrDefault(req.Actor), "composition.create", m.ID, map[string]any{
		"name": m.Name, "members": len(m.Members),
	})
	s.log.Info().Str("composition_id", m.ID).Str("actor", actorOrDefault(req.Actor)).Msg("composition created")
	writeJSON(w, http.StatusCreated, map[string]any{"composition": m})
}

// ---------------------------------------------------------------------------
// PUT /api/v1/compositions/{id}
// ---------------------------------------------------------------------------

func (s *Server) handleCompositionUpdate(w http.ResponseWriter, r *http.Request) {
	if s.compositions == nil {
		writeError(w, http.StatusServiceUnavailable, CodeInternal, "composition store not configured")
		return
	}
	id := strings.TrimSpace(chi.URLParam(r, "id"))
	var req compositionRequest
	if err := decodeStrictJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeValidation, err.Error())
		return
	}
	m := req.toComposition()
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
	if err := s.compositions.Update(r.Context(), m); errors.Is(err, composition.ErrNotFound) {
		writeError(w, http.StatusNotFound, CodeNotFound, fmt.Sprintf("composition %q not found", id))
		return
	} else if err != nil {
		internalError(w, s.log, "composition update", err)
		return
	}
	s.auditComposition(r, actorOrDefault(req.Actor), "composition.update", m.ID, map[string]any{
		"name": m.Name, "members": len(m.Members),
	})
	s.log.Info().Str("composition_id", m.ID).Str("actor", actorOrDefault(req.Actor)).Msg("composition updated")
	writeJSON(w, http.StatusOK, map[string]any{"composition": m})
}

// ---------------------------------------------------------------------------
// DELETE /api/v1/compositions/{id}
// ---------------------------------------------------------------------------

func (s *Server) handleCompositionDelete(w http.ResponseWriter, r *http.Request) {
	if s.compositions == nil {
		writeError(w, http.StatusServiceUnavailable, CodeInternal, "composition store not configured")
		return
	}
	id := strings.TrimSpace(chi.URLParam(r, "id"))
	actor := actorOrDefault(actorFromQuery(r))
	if err := s.compositions.Delete(r.Context(), id); errors.Is(err, composition.ErrNotFound) {
		writeError(w, http.StatusNotFound, CodeNotFound, fmt.Sprintf("composition %q not found", id))
		return
	} else if err != nil {
		internalError(w, s.log, "composition delete", err)
		return
	}
	s.auditComposition(r, actor, "composition.delete", id, nil)
	s.log.Info().Str("composition_id", id).Str("actor", actor).Msg("composition deleted")
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// auditComposition appends one tms.audit_log row for a Composition mutation
// (best-effort: a write failure is logged, not surfaced — the mutation already
// committed).
func (s *Server) auditComposition(r *http.Request, actor, action, compositionID string, details map[string]any) {
	if s.auditLog == nil {
		return
	}
	if err := s.auditLog.WriteAudit(r.Context(), AuditRecord{
		Actor: actor, Action: action, Entity: "composition", EntityID: compositionID, Details: details,
	}); err != nil {
		s.log.Warn().Err(err).Str("action", action).Str("composition_id", compositionID).
			Msg("composition audit write failed; mutation already committed")
	}
}

// actorFromQuery reads the optional ?actor= for DELETE (which carries no JSON
// body to hold an actor field).
func actorFromQuery(r *http.Request) string {
	return strings.TrimSpace(r.URL.Query().Get("actor"))
}
