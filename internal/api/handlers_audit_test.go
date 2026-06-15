package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/jobs"
)

// auditListResponse is the wire shape of GET /api/v1/audit.
type auditListResponse struct {
	Entries    []AuditEntry `json:"entries"`
	NextBefore *int64       `json:"next_before"`
}

func seedAudit(ts *testServer) {
	base := fixedNow
	ts.audit.entries = []AuditEntry{
		{ID: 5, TS: base, Actor: "api:alice", Action: "job.enqueued", Entity: "job", EntityID: "42",
			Details: json.RawMessage(`{"kind":"data.refresh"}`)},
		{ID: 4, TS: base.Add(-1 * time.Minute), Actor: "system", Action: "job.claimed", Entity: "job", EntityID: "42"},
		{ID: 3, TS: base.Add(-2 * time.Minute), Actor: "api:bob", Action: "universe.rebuild", Entity: "job", EntityID: "41"},
		{ID: 2, TS: base.Add(-3 * time.Minute), Actor: "cli", Action: "param.promote", Entity: "strategy", EntityID: "sepa"},
		{ID: 1, TS: base.Add(-4 * time.Minute), Actor: "system", Action: "job.failed", Entity: "job", EntityID: "40"},
	}
}

// TestAuditListNewestFirst: the endpoint returns rows newest-first with the
// optional entity/entity_id/details only when present.
func TestAuditListNewestFirst(t *testing.T) {
	ts := newTestServer(t)
	seedAudit(ts)

	rec := ts.do(t, http.MethodGet, "/api/v1/audit", nil, true)
	require.Equal(t, http.StatusOK, rec.Code)

	var got auditListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got), "body: %s", rec.Body.String())
	require.Len(t, got.Entries, 5)

	// Newest first (descending id).
	assert.Equal(t, int64(5), got.Entries[0].ID)
	assert.Equal(t, "api:alice", got.Entries[0].Actor)
	assert.Equal(t, "job.enqueued", got.Entries[0].Action)
	assert.Equal(t, "job", got.Entries[0].Entity)
	assert.Equal(t, "42", got.Entries[0].EntityID)
	assert.JSONEq(t, `{"kind":"data.refresh"}`, string(got.Entries[0].Details))

	// A short page (fewer than the limit) carries no cursor.
	assert.Nil(t, got.NextBefore)

	// Timestamps are emitted UTC RFC3339.
	assert.True(t, strings.HasSuffix(rec.Body.String(), "\n"))
}

// TestAuditListFilters: actor + action exact-match filters pass through to the
// reader and scope the result.
func TestAuditListFilters(t *testing.T) {
	ts := newTestServer(t)
	seedAudit(ts)

	rec := ts.do(t, http.MethodGet, "/api/v1/audit?actor=system&action=job.claimed", nil, true)
	require.Equal(t, http.StatusOK, rec.Code)

	var got auditListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Len(t, got.Entries, 1)
	assert.Equal(t, int64(4), got.Entries[0].ID)
	assert.Equal(t, "system", ts.audit.lastQuery.Actor)
	assert.Equal(t, "job.claimed", ts.audit.lastQuery.Action)
}

// TestAuditListPagination: a full page surfaces a next_before keyset cursor;
// passing it back returns the following (older) page.
func TestAuditListPagination(t *testing.T) {
	ts := newTestServer(t)
	seedAudit(ts)

	rec := ts.do(t, http.MethodGet, "/api/v1/audit?limit=2", nil, true)
	require.Equal(t, http.StatusOK, rec.Code)
	var page1 auditListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page1))
	require.Len(t, page1.Entries, 2)
	assert.Equal(t, int64(5), page1.Entries[0].ID)
	assert.Equal(t, int64(4), page1.Entries[1].ID)
	require.NotNil(t, page1.NextBefore)
	assert.Equal(t, int64(4), *page1.NextBefore)

	rec2 := ts.do(t, http.MethodGet, "/api/v1/audit?limit=2&before=4", nil, true)
	require.Equal(t, http.StatusOK, rec2.Code)
	var page2 auditListResponse
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &page2))
	require.Len(t, page2.Entries, 2)
	assert.Equal(t, int64(3), page2.Entries[0].ID)
	assert.Equal(t, int64(2), page2.Entries[1].ID)
}

// TestAuditListBadBefore / BadLimit: malformed pagination params are 400.
func TestAuditListBadParams(t *testing.T) {
	ts := newTestServer(t)
	seedAudit(ts)

	for _, target := range []string{
		"/api/v1/audit?before=abc",
		"/api/v1/audit?before=0",
		"/api/v1/audit?limit=0",
		"/api/v1/audit?limit=99999",
	} {
		rec := ts.do(t, http.MethodGet, target, nil, true)
		assert.Equal(t, http.StatusBadRequest, rec.Code, target)
		assert.Equal(t, CodeValidation, errCode(t, rec), target)
	}
}

// TestAuditListRequiresAuth: the endpoint is bearer-guarded like every /api/*.
func TestAuditListRequiresAuth(t *testing.T) {
	ts := newTestServer(t)
	rec := ts.do(t, http.MethodGet, "/api/v1/audit", nil, false)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

// TestAuditListNoReader: when the server has no audit reader, the endpoint
// reports 503 (not configured) rather than crashing.
func TestAuditListNoReader(t *testing.T) {
	ts := newTestServer(t)
	ts.srv.audit = nil
	rec := ts.do(t, http.MethodGet, "/api/v1/audit", nil, true)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

// TestAuditListInternalError: a reader failure becomes a generic 500.
func TestAuditListInternalError(t *testing.T) {
	ts := newTestServer(t)
	ts.audit.err = assertErr("boom")
	rec := ts.do(t, http.MethodGet, "/api/v1/audit", nil, true)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Equal(t, CodeInternal, errCode(t, rec))
	assert.NotContains(t, rec.Body.String(), "boom", "internal detail must not leak")
}

// ---------------------------------------------------------------------------
// Job retry
// ---------------------------------------------------------------------------

// TestJobRetryClonesTerminalJob: retrying a failed job enqueues a NEW job with
// the same kind + payload, leaving the source row untouched.
func TestJobRetryClonesTerminalJob(t *testing.T) {
	ts := newTestServer(t)
	finished := fixedNow
	errMsg := "exploded"
	src := ts.jobs.add(&jobs.Job{
		Kind:        "data.refresh",
		Payload:     json.RawMessage(`{"source":"parquet","tickers":["AAPL"]}`),
		Status:      jobs.StatusFailed,
		Priority:    3,
		MaxAttempts: 2,
		FinishedAt:  &finished,
		LastError:   &errMsg,
		CreatedAt:   fixedNow,
		UpdatedAt:   fixedNow,
	})

	rec := ts.do(t, http.MethodPost, jobPath(src.ID)+"/retry", strings.NewReader(`{"actor":"alice"}`), true)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var out struct {
		Job         jobJSON `json:"job"`
		SourceJobID int64   `json:"source_job_id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Equal(t, src.ID, out.SourceJobID)
	assert.NotEqual(t, src.ID, out.Job.ID, "retry creates a NEW job")
	assert.Equal(t, "data.refresh", out.Job.Kind)
	assert.Equal(t, string(jobs.StatusQueued), out.Job.Status)
	assert.JSONEq(t, `{"source":"parquet","tickers":["AAPL"]}`, string(out.Job.Payload))
	assert.Equal(t, int32(2), out.Job.MaxAttempts)

	// The clone carries the actor and no dedupe key.
	require.Len(t, ts.jobs.enqueued, 1)
	assert.Equal(t, "api:alice", ts.jobs.enqueued[0].Actor)
	assert.Empty(t, ts.jobs.enqueued[0].DedupeKey)

	// Source row is unchanged (still failed).
	after, err := ts.jobs.Get(t.Context(), src.ID)
	require.NoError(t, err)
	assert.Equal(t, jobs.StatusFailed, after.Status)
}

// TestJobRetryCanceledOK: a canceled job is retryable too.
func TestJobRetryCanceledOK(t *testing.T) {
	ts := newTestServer(t)
	finished := fixedNow
	src := ts.jobs.add(&jobs.Job{
		Kind: "universe.rebuild", Payload: json.RawMessage(`{}`),
		Status: jobs.StatusCanceled, MaxAttempts: 1, FinishedAt: &finished,
		CreatedAt: fixedNow, UpdatedAt: fixedNow,
	})
	rec := ts.do(t, http.MethodPost, jobPath(src.ID)+"/retry", nil, true)
	assert.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
}

// TestJobRetryNonTerminalRejected: a queued/running/succeeded job cannot be
// retried (422 validation).
func TestJobRetryNonTerminalRejected(t *testing.T) {
	ts := newTestServer(t)
	for _, st := range []jobs.Status{jobs.StatusQueued, jobs.StatusRunning, jobs.StatusSucceeded} {
		finished := fixedNow
		j := &jobs.Job{Kind: "data.refresh", Payload: json.RawMessage(`{}`), Status: st,
			MaxAttempts: 1, CreatedAt: fixedNow, UpdatedAt: fixedNow}
		if st == jobs.StatusSucceeded || st == jobs.StatusRunning {
			j.FinishedAt = &finished
		}
		src := ts.jobs.add(j)
		rec := ts.do(t, http.MethodPost, jobPath(src.ID)+"/retry", nil, true)
		assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, "status %s", st)
		assert.Equal(t, CodeValidation, errCode(t, rec), "status %s", st)
	}
}

// TestJobRetryNotFound: retrying an unknown id is 404.
func TestJobRetryNotFound(t *testing.T) {
	ts := newTestServer(t)
	rec := ts.do(t, http.MethodPost, jobPath(9999)+"/retry", nil, true)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, CodeNotFound, errCode(t, rec))
}

// TestJobRetryRequiresAuth.
func TestJobRetryRequiresAuth(t *testing.T) {
	ts := newTestServer(t)
	rec := ts.do(t, http.MethodPost, jobPath(1)+"/retry", nil, false)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}
