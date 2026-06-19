package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/byjackchen/trade-tms-go/internal/jobs"
)

const (
	defaultJobsLimit = 50
	maxJobsLimit     = 500
)

// jobJSON is the wire shape of one job (docs/reference/api.md).
type jobJSON struct {
	ID              int64           `json:"id"`
	Kind            string          `json:"kind"`
	Status          string          `json:"status"`
	Payload         json.RawMessage `json:"payload"`
	Priority        int32           `json:"priority"`
	RunAt           time.Time       `json:"run_at"`
	Attempts        int32           `json:"attempts"`
	MaxAttempts     int32           `json:"max_attempts"`
	DedupeKey       *string         `json:"dedupe_key"`
	ClaimedBy       *string         `json:"claimed_by"`
	ClaimedAt       *time.Time      `json:"claimed_at"`
	HeartbeatAt     *time.Time      `json:"heartbeat_at"`
	StartedAt       *time.Time      `json:"started_at"`
	FinishedAt      *time.Time      `json:"finished_at"`
	LastError       *string         `json:"last_error"`
	Progress        json.RawMessage `json:"progress,omitempty"`
	CancelRequested bool            `json:"cancel_requested"`
	Result          json.RawMessage `json:"result,omitempty"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
}

func jobToJSON(j *jobs.Job) jobJSON {
	return jobJSON{
		ID:              j.ID,
		Kind:            j.Kind,
		Status:          string(j.Status),
		Payload:         j.Payload,
		Priority:        j.Priority,
		RunAt:           j.RunAt.UTC(),
		Attempts:        j.Attempts,
		MaxAttempts:     j.MaxAttempts,
		DedupeKey:       j.DedupeKey,
		ClaimedBy:       j.ClaimedBy,
		ClaimedAt:       utcPtr(j.ClaimedAt),
		HeartbeatAt:     utcPtr(j.HeartbeatAt),
		StartedAt:       utcPtr(j.StartedAt),
		FinishedAt:      utcPtr(j.FinishedAt),
		LastError:       j.LastError,
		Progress:        j.Progress,
		CancelRequested: j.CancelRequested,
		Result:          j.Result,
		CreatedAt:       j.CreatedAt.UTC(),
		UpdatedAt:       j.UpdatedAt.UTC(),
	}
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// validStatuses for the ?status= filter.
var validStatuses = map[jobs.Status]struct{}{
	jobs.StatusQueued: {}, jobs.StatusRunning: {}, jobs.StatusSucceeded: {},
	jobs.StatusFailed: {}, jobs.StatusCanceled: {},
}

// GET /api/v1/jobs
func (s *Server) handleJobList(w http.ResponseWriter, r *http.Request) {
	status := jobs.Status(strings.TrimSpace(r.URL.Query().Get("status")))
	if status != "" {
		if _, ok := validStatuses[status]; !ok {
			writeError(w, http.StatusBadRequest, CodeValidation,
				fmt.Sprintf("unknown status %q (want queued|running|succeeded|failed|canceled)", status))
			return
		}
	}
	limit, ok := parseLimit(w, r, defaultJobsLimit, maxJobsLimit)
	if !ok {
		return
	}
	list, err := s.jobs.List(r.Context(), jobs.ListFilter{
		Kind:   strings.TrimSpace(r.URL.Query().Get("kind")),
		Status: status,
		Limit:  int32(limit),
	})
	if err != nil {
		internalError(w, s.log, "job list", err)
		return
	}
	out := make([]jobJSON, 0, len(list))
	for _, j := range list {
		out = append(out, jobToJSON(j))
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": out})
}

// jobIDParam parses {id}, writing the 400 itself on garbage.
func jobIDParam(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := chi.URLParam(r, "id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, CodeValidation,
			fmt.Sprintf("invalid job id %q (want a positive integer)", raw))
		return 0, false
	}
	return id, true
}

// GET /api/v1/jobs/{id}
func (s *Server) handleJobGet(w http.ResponseWriter, r *http.Request) {
	id, ok := jobIDParam(w, r)
	if !ok {
		return
	}
	job, err := s.jobs.Get(r.Context(), id)
	if errors.Is(err, jobs.ErrNotFound) {
		writeError(w, http.StatusNotFound, CodeNotFound, fmt.Sprintf("job %d not found", id))
		return
	}
	if err != nil {
		internalError(w, s.log, "job get", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": jobToJSON(job)})
}

// cancelRequest is the optional POST body of the cancel endpoint.
type cancelRequest struct {
	Reason string `json:"reason"`
	Actor  string `json:"actor"`
}

// POST /api/v1/jobs/{id}/cancel
func (s *Server) handleJobCancel(w http.ResponseWriter, r *http.Request) {
	id, ok := jobIDParam(w, r)
	if !ok {
		return
	}
	var req cancelRequest
	if err := decodeStrictJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeValidation, err.Error())
		return
	}
	outcome, job, err := s.jobs.Cancel(r.Context(), id, actorOrDefault(req.Actor), strings.TrimSpace(req.Reason))
	if errors.Is(err, jobs.ErrNotFound) {
		writeError(w, http.StatusNotFound, CodeNotFound, fmt.Sprintf("job %d not found", id))
		return
	}
	if err != nil {
		internalError(w, s.log, "job cancel", err)
		return
	}
	s.log.Info().Int64("job_id", id).Str("outcome", string(outcome)).Msg("job cancel requested via api")
	writeJSON(w, http.StatusOK, map[string]any{"outcome": string(outcome), "job": jobToJSON(job)})
}

// retryRequest is the optional POST body of the retry endpoint.
type retryRequest struct {
	Actor string `json:"actor"`
}

// terminalForRetry are the statuses a job can be retried from: a job that has
// finished unsuccessfully. A queued/running job is still in flight; a succeeded
// job has nothing to retry.
var terminalForRetry = map[jobs.Status]struct{}{
	jobs.StatusFailed:   {},
	jobs.StatusCanceled: {},
}

// POST /api/v1/jobs/{id}/retry
//
// Re-enqueues a NEW job cloning the original's kind + payload. This never
// mutates the source row (the audit trail and the failed/canceled record are
// preserved); it is the same enqueue path the original used, so the worker
// re-validates the payload. Only failed/canceled jobs are retryable (a
// queued/running job is in flight; a succeeded job has nothing to retry).
//
// The clone carries no dedupe_key (a manual retry is an explicit operator
// action — it must not be silently deduped against a still-active job) and
// inherits the source's max_attempts. Optional body: {"actor": "..."}.
func (s *Server) handleJobRetry(w http.ResponseWriter, r *http.Request) {
	id, ok := jobIDParam(w, r)
	if !ok {
		return
	}
	var req retryRequest
	if err := decodeStrictJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeValidation, err.Error())
		return
	}

	src, err := s.jobs.Get(r.Context(), id)
	if errors.Is(err, jobs.ErrNotFound) {
		writeError(w, http.StatusNotFound, CodeNotFound, fmt.Sprintf("job %d not found", id))
		return
	}
	if err != nil {
		internalError(w, s.log, "job retry get", err)
		return
	}
	if _, retryable := terminalForRetry[src.Status]; !retryable {
		writeError(w, http.StatusUnprocessableEntity, CodeValidation,
			fmt.Sprintf("job %d is %s; only failed or canceled jobs can be retried", id, src.Status))
		return
	}

	// Clone kind + payload verbatim. Payload is a JSON object (the schema CHECK
	// guarantees it); pass the raw message straight through so the new job is a
	// faithful re-run of the original input.
	payload := json.RawMessage(src.Payload)
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	clone, _, err := s.jobs.Enqueue(r.Context(), jobs.EnqueueParams{
		Kind:        src.Kind,
		Payload:     payload,
		Priority:    src.Priority,
		MaxAttempts: src.MaxAttempts,
		Actor:       actorOrDefault(req.Actor),
	})
	if err != nil {
		internalError(w, s.log, "job retry enqueue", err)
		return
	}
	s.log.Info().Int64("source_job_id", id).Int64("retry_job_id", clone.ID).
		Str("kind", src.Kind).Msg("job retried via api")
	writeJSON(w, http.StatusCreated, map[string]any{
		"job":           jobToJSON(clone),
		"source_job_id": id,
	})
}
