package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/data/universe"
	"github.com/byjackchen/trade-tms-go/internal/jobs"
	"github.com/byjackchen/trade-tms-go/internal/jobs/handlers"
)

// snapshotJSON is the wire shape of one universe snapshot (docs/reference/api.md).
type snapshotJSON struct {
	ID          int64             `json:"id"`
	AsOf        string            `json:"as_of"`
	Kind        string            `json:"kind"`
	TableFilter string            `json:"table_filter,omitempty"`
	WindowStart string            `json:"window_start,omitempty"`
	WindowEnd   string            `json:"window_end,omitempty"`
	LimitN      int               `json:"limit_n"`
	Tickers     []string          `json:"tickers"`
	Excluded    []string          `json:"excluded"`
	Params      map[string]any    `json:"params"`
	Members     []universe.Member `json:"members"`
	CreatedAt   time.Time         `json:"created_at"`
}

func snapshotToJSON(snap *universe.Snapshot) snapshotJSON {
	out := snapshotJSON{
		ID:          snap.ID,
		AsOf:        snap.AsOf.String(),
		Kind:        snap.Kind,
		TableFilter: snap.TableFilter,
		LimitN:      snap.LimitN,
		Tickers:     snap.Tickers,
		Excluded:    snap.Excluded,
		Params:      snap.Params,
		Members:     snap.Members,
		CreatedAt:   snap.CreatedAt.UTC(),
	}
	if !snap.WindowStart.IsZero() {
		out.WindowStart = snap.WindowStart.String()
	}
	if !snap.WindowEnd.IsZero() {
		out.WindowEnd = snap.WindowEnd.String()
	}
	if out.Tickers == nil {
		out.Tickers = []string{}
	}
	if out.Excluded == nil {
		out.Excluded = []string{}
	}
	if out.Members == nil {
		out.Members = []universe.Member{}
	}
	if out.Params == nil {
		out.Params = map[string]any{}
	}
	return out
}

func validSnapshotKind(kind string) bool {
	switch kind {
	case universe.KindLive, universe.KindEOD, universe.KindBacktest, universe.KindManual:
		return true
	}
	return false
}

// GET /api/v1/universe/latest
func (s *Server) handleUniverseLatest(w http.ResponseWriter, r *http.Request) {
	kind := strings.TrimSpace(r.URL.Query().Get("kind"))
	if kind != "" && !validSnapshotKind(kind) {
		writeError(w, http.StatusBadRequest, CodeValidation,
			fmt.Sprintf("unknown kind %q (want live|eod|backtest|manual)", kind))
		return
	}
	snap, err := s.uni.LatestSnapshot(r.Context(), kind)
	if errors.Is(err, universe.ErrNoSnapshot) {
		msg := "no universe snapshot exists yet"
		if kind != "" {
			msg = fmt.Sprintf("no universe snapshot of kind %q exists yet", kind)
		}
		writeError(w, http.StatusNotFound, CodeNotFound, msg)
		return
	}
	if err != nil {
		internalError(w, s.log, "universe latest", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshot": snapshotToJSON(snap)})
}

// rebuildRequest is the POST /universe/rebuild body. Limit is a pointer so
// "absent" (worker resolves TMS_LIVE_UNIVERSE_LIMIT, default 85) differs
// from an explicit 0 (empty universe, reference semantics).
type rebuildRequest struct {
	Kind     string `json:"kind"`
	Limit    *int   `json:"limit"`
	Uncapped bool   `json:"uncapped"`
	TopK     int    `json:"top_k"`
	Actor    string `json:"actor"`
}

// POST /api/v1/universe/rebuild
func (s *Server) handleUniverseRebuild(w http.ResponseWriter, r *http.Request) {
	var req rebuildRequest
	if err := decodeStrictJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeValidation, err.Error())
		return
	}
	if req.Kind == "" {
		req.Kind = universe.KindManual
	}
	if !validSnapshotKind(req.Kind) {
		writeError(w, http.StatusBadRequest, CodeValidation,
			fmt.Sprintf("unknown kind %q (want live|eod|backtest|manual)", req.Kind))
		return
	}
	if req.TopK < 0 {
		writeError(w, http.StatusBadRequest, CodeValidation, "top_k must be >= 0")
		return
	}

	payload := map[string]any{
		"kind":     req.Kind,
		"uncapped": req.Uncapped,
		"top_k":    req.TopK,
	}
	if req.Limit != nil {
		payload["limit"] = *req.Limit
	}

	job, deduped, err := s.jobs.Enqueue(r.Context(), jobs.EnqueueParams{
		Kind:      handlers.KindUniverseRebuild,
		Payload:   payload,
		DedupeKey: dedupeUniverseRebuild,
		Actor:     actorOrDefault(req.Actor),
	})
	if err != nil {
		internalError(w, s.log, "enqueue universe.rebuild", err)
		return
	}
	s.log.Info().Int64("job_id", job.ID).Str("kind", req.Kind).
		Bool("deduped", deduped).Msg("universe.rebuild enqueued")
	writeJSON(w, http.StatusAccepted, map[string]any{"job": jobToJSON(job), "deduped": deduped})
}
