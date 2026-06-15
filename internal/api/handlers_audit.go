package api

// handlers_audit.go serves GET /api/v1/audit: the paginated, newest-first view
// of tms.audit_log (the append-only operational audit trail — param promotions,
// halts, job lifecycle, manual interventions). The Ops UI's AUDIT LOG panel
// renders this stream. Reads only; the trail is written atomically with the
// state changes it records (see internal/jobs/queue.go audit(), commands, etc.).

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	defaultAuditLimit = 50
	maxAuditLimit     = 500
)

// AuditEntry is one tms.audit_log row (the wire shape of an audit record). The
// optional entity/entity_id/details are emitted only when present so the UI can
// render a target ("job #42") and an expandable detail blob.
type AuditEntry struct {
	ID       int64           `json:"id"`
	TS       time.Time       `json:"ts"`
	Actor    string          `json:"actor"`
	Action   string          `json:"action"`
	Entity   string          `json:"entity,omitempty"`
	EntityID string          `json:"entity_id,omitempty"`
	Details  json.RawMessage `json:"details,omitempty"`
}

// AuditFilter scopes an audit-log query. All fields are optional; zero values
// match everything.
type AuditFilter struct {
	Actor    string // exact actor match ("" = any)
	Action   string // exact action match ("" = any)
	Entity   string // exact entity-kind match ("" = any)
	EntityID string // exact entity-id match ("" = any)
	Before   *int64 // keyset cursor: only rows with id < Before (nil = newest)
	Limit    int
}

// AuditReader reads the append-only tms.audit_log, newest-first (satisfied by
// *apistore.PGStore). It backs GET /api/v1/audit.
type AuditReader interface {
	Audit(ctx context.Context, f AuditFilter) ([]AuditEntry, error)
}

// GET /api/v1/audit — paginated, newest-first audit trail.
//
//   - actor / action / entity / entity_id (optional): exact-match filters.
//   - before (optional): keyset cursor — return rows with id strictly less than
//     this id (the id of the oldest row already shown). Page by passing the last
//     row's id back as ?before=.
//   - limit (optional): default 50, range [1, 500].
func (s *Server) handleAuditList(w http.ResponseWriter, r *http.Request) {
	if s.audit == nil {
		// No audit reader wired (a deployment choice): report it explicitly so
		// the UI renders a "not configured" empty state rather than spinning.
		writeError(w, http.StatusServiceUnavailable, CodeValidation,
			"audit log reader not configured")
		return
	}
	limit, ok := parseLimit(w, r, defaultAuditLimit, maxAuditLimit)
	if !ok {
		return
	}
	q := r.URL.Query()
	f := AuditFilter{
		Actor:    strings.TrimSpace(q.Get("actor")),
		Action:   strings.TrimSpace(q.Get("action")),
		Entity:   strings.TrimSpace(q.Get("entity")),
		EntityID: strings.TrimSpace(q.Get("entity_id")),
		Limit:    limit,
	}
	if raw := strings.TrimSpace(q.Get("before")); raw != "" {
		before, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || before < 1 {
			writeError(w, http.StatusBadRequest, CodeValidation,
				"before must be a positive integer audit id")
			return
		}
		f.Before = &before
	}

	entries, err := s.audit.Audit(r.Context(), f)
	if err != nil {
		internalError(w, s.log, "audit list", err)
		return
	}
	out := make([]AuditEntry, 0, len(entries))
	out = append(out, entries...)

	// next_before is the keyset cursor for the following (older) page: the id of
	// the last (oldest) row in this page, or null when the page is short of the
	// limit (no more rows).
	var nextBefore *int64
	if len(out) == limit && limit > 0 {
		id := out[len(out)-1].ID
		nextBefore = &id
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entries":     out,
		"next_before": nextBefore,
	})
}
