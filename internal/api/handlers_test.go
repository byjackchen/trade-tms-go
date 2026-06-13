package api

// handlers_test.go is the REST contract suite: every endpoint is exercised
// for auth (401), input validation (400 / 404), and a happy path against the
// in-memory stubs from stub_test.go. It is the executable companion to
// docs/api.md.

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/data/universe"
	"github.com/byjackchen/trade-tms-go/internal/jobs"
)

func d(t *testing.T, s string) calendar.Date {
	t.Helper()
	dt, err := calendar.ParseDate(s)
	require.NoError(t, err)
	return dt
}

// ---------------------------------------------------------------------------
// constructor validation
// ---------------------------------------------------------------------------

func TestNewServer_RejectsMissingDeps(t *testing.T) {
	cal, err := calendar.NewNYSE()
	require.NoError(t, err)
	base := Deps{
		Token: testToken, Jobs: newStubJobQueue(),
		Data: &stubDataStore{}, Universe: &stubUniverseReader{},
		Calendar: cal, PingPG: pingOK,
	}
	t.Run("empty token", func(t *testing.T) {
		d := base
		d.Token = ""
		_, err := NewServer(d)
		require.Error(t, err)
	})
	t.Run("nil pg ping", func(t *testing.T) {
		d := base
		d.PingPG = nil
		_, err := NewServer(d)
		require.Error(t, err)
	})
	t.Run("nil calendar", func(t *testing.T) {
		d := base
		d.Calendar = nil
		_, err := NewServer(d)
		require.Error(t, err)
	})
}

// ---------------------------------------------------------------------------
// auth
// ---------------------------------------------------------------------------

func TestAuth(t *testing.T) {
	ts := newTestServer(t)

	t.Run("healthz is public", func(t *testing.T) {
		rec := ts.do(t, http.MethodGet, "/healthz", nil, false)
		assert.Equal(t, http.StatusOK, rec.Code)
	})
	t.Run("version is public", func(t *testing.T) {
		rec := ts.do(t, http.MethodGet, "/version", nil, false)
		assert.Equal(t, http.StatusOK, rec.Code)
	})
	t.Run("api route without token is 401", func(t *testing.T) {
		rec := ts.do(t, http.MethodGet, "/api/v1/jobs", nil, false)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
		assert.Equal(t, CodeUnauthorized, errCode(t, rec))
		assert.NotEmpty(t, rec.Header().Get("WWW-Authenticate"))
	})
	t.Run("api route with wrong token is 401", func(t *testing.T) {
		rec := ts.doToken(t, http.MethodGet, "/api/v1/jobs", "wrong-token")
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})
	t.Run("api route with valid token passes", func(t *testing.T) {
		rec := ts.do(t, http.MethodGet, "/api/v1/jobs", nil, true)
		assert.Equal(t, http.StatusOK, rec.Code)
	})
	t.Run("token via query parameter (ws clients)", func(t *testing.T) {
		rec := ts.do(t, http.MethodGet, "/api/v1/jobs?token="+testToken, nil, false)
		assert.Equal(t, http.StatusOK, rec.Code)
	})
}

// ---------------------------------------------------------------------------
// CORS
// ---------------------------------------------------------------------------

func TestCORS(t *testing.T) {
	ts := newTestServer(t)
	t.Run("allowlisted origin echoed on preflight", func(t *testing.T) {
		rec := ts.doPreflight(t, "/api/v1/jobs", testOrigin)
		assert.Equal(t, http.StatusNoContent, rec.Code)
		assert.Equal(t, testOrigin, rec.Header().Get("Access-Control-Allow-Origin"))
	})
	t.Run("non-allowlisted origin gets no CORS header", func(t *testing.T) {
		rec := ts.doPreflight(t, "/api/v1/jobs", "http://evil.example")
		assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
	})
}

// ---------------------------------------------------------------------------
// GET /healthz
// ---------------------------------------------------------------------------

func TestHealthz(t *testing.T) {
	t.Run("all deps ok", func(t *testing.T) {
		ts := newTestServer(t)
		rec := ts.do(t, http.MethodGet, "/healthz", nil, false)
		assert.Equal(t, http.StatusOK, rec.Code)
		body := decodeBody(t, rec)
		assert.Equal(t, "ok", body["status"])
		deps := body["deps"].(map[string]any)
		assert.True(t, deps["postgres"].(map[string]any)["ok"].(bool))
		assert.True(t, deps["redis"].(map[string]any)["ok"].(bool))
	})
	t.Run("degraded when a dep is down (still HTTP 200)", func(t *testing.T) {
		cal, _ := calendar.NewNYSE()
		srv, err := NewServer(Deps{
			Log: zerolog.Nop(), Token: testToken, CORSOrigins: []string{testOrigin},
			Jobs: newStubJobQueue(), Data: &stubDataStore{}, Universe: &stubUniverseReader{},
			Calendar: cal, PingPG: pingOK, PingRedis: pingErr,
			Now: func() time.Time { return fixedNow },
		})
		require.NoError(t, err)
		ts := &testServer{srv: srv}
		rec := ts.do(t, http.MethodGet, "/healthz", nil, false)
		assert.Equal(t, http.StatusOK, rec.Code)
		body := decodeBody(t, rec)
		assert.Equal(t, "degraded", body["status"])
		assert.False(t, body["deps"].(map[string]any)["redis"].(map[string]any)["ok"].(bool))
	})
}

// ---------------------------------------------------------------------------
// GET /api/v1/data/coverage
// ---------------------------------------------------------------------------

func TestCoverage(t *testing.T) {
	ts := newTestServer(t)
	// bars_daily: BAC has a one-day gap (2024-06-10 is a session it is
	// missing); AAPL is complete over its short span.
	ts.data.coverage = []TableCoverage{
		{Table: "tickers", Rows: 2, Tickers: 2, MinDate: d(t, "2020-01-02"), MaxDate: d(t, "2024-06-11")},
		{Table: "bars_daily", Rows: 6, Tickers: 2, MinDate: d(t, "2024-06-06"), MaxDate: d(t, "2024-06-11")},
	}
	ts.data.spans = []TickerSpan{
		// AAPL: 4 sessions 6/6,6/7,6/10,6/11 → 4 bars, no gap.
		{Ticker: "AAPL", Bars: 4, First: d(t, "2024-06-06"), Last: d(t, "2024-06-11")},
		// BAC: span 6/6..6/11 (4 sessions) but only 2 bars → 2 missing days.
		{Ticker: "BAC", Bars: 2, First: d(t, "2024-06-06"), Last: d(t, "2024-06-11")},
	}

	rec := ts.do(t, http.MethodGet, "/api/v1/data/coverage", nil, true)
	require.Equal(t, http.StatusOK, rec.Code)
	body := decodeBody(t, rec)

	// Latest NYSE session for fixedNow (Wed 2024-06-12) is 2024-06-12.
	assert.Equal(t, "2024-06-12", body["latest_session"])

	tables := body["tables"].([]any)
	require.Len(t, tables, 2)
	bars := findTable(t, tables, "bars_daily")
	// max_date 2024-06-11 → lag of one session (06-12).
	fresh := bars["freshness"].(map[string]any)
	assert.Equal(t, float64(1), fresh["lag_sessions"])
	assert.Equal(t, "2024-06-12", fresh["latest_session"])

	gaps := bars["gaps"].(map[string]any)
	assert.Equal(t, float64(2), gaps["tickers_scanned"])
	assert.Equal(t, float64(1), gaps["tickers_with_gaps"])
	worst := gaps["worst"].([]any)
	require.Len(t, worst, 1)
	assert.Equal(t, "BAC", worst[0].(map[string]any)["ticker"])
}

func TestCoverage_DBError(t *testing.T) {
	ts := newTestServer(t)
	ts.data.coverageErr = assertErr("boom")
	rec := ts.do(t, http.MethodGet, "/api/v1/data/coverage", nil, true)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Equal(t, CodeInternal, errCode(t, rec))
	// Internal error text must never leak the underlying message.
	assert.NotContains(t, rec.Body.String(), "boom")
}

func TestCoverage_TickerDrilldown(t *testing.T) {
	ts := newTestServer(t)
	// MSFT present with a gap: has 6/6 and 6/11 but is missing 6/7 and 6/10.
	ts.data.tickers["MSFT"] = true
	ts.data.barDates["MSFT"] = []calendar.Date{d(t, "2024-06-06"), d(t, "2024-06-11")}

	t.Run("missing days listed", func(t *testing.T) {
		rec := ts.do(t, http.MethodGet, "/api/v1/data/coverage?ticker=msft", nil, true)
		require.Equal(t, http.StatusOK, rec.Code)
		body := decodeBody(t, rec)
		assert.Equal(t, "MSFT", body["ticker"])
		assert.Equal(t, float64(2), body["missing_days"])
		missing := body["missing"].([]any)
		assert.ElementsMatch(t, []any{"2024-06-07", "2024-06-10"}, missing)
		assert.False(t, body["missing_truncated"].(bool))
	})

	t.Run("unknown ticker is 404", func(t *testing.T) {
		rec := ts.do(t, http.MethodGet, "/api/v1/data/coverage?ticker=NOPE", nil, true)
		assert.Equal(t, http.StatusNotFound, rec.Code)
		assert.Equal(t, CodeNotFound, errCode(t, rec))
	})

	t.Run("known ticker with no bars", func(t *testing.T) {
		ts.data.tickers["EMPTY"] = true
		rec := ts.do(t, http.MethodGet, "/api/v1/data/coverage?ticker=EMPTY", nil, true)
		require.Equal(t, http.StatusOK, rec.Code)
		body := decodeBody(t, rec)
		assert.Equal(t, float64(0), body["bars"])
	})
}

func findTable(t *testing.T, tables []any, name string) map[string]any {
	t.Helper()
	for _, raw := range tables {
		m := raw.(map[string]any)
		if m["table"] == name {
			return m
		}
	}
	t.Fatalf("table %q not found", name)
	return nil
}

// ---------------------------------------------------------------------------
// GET /api/v1/data/tickers
// ---------------------------------------------------------------------------

func TestTickerSearch(t *testing.T) {
	ts := newTestServer(t)
	ts.data.search = []TickerMeta{
		{Ticker: "AAPL", Name: "Apple Inc", Exchange: "NASDAQ", Table: "SEP"},
	}
	t.Run("missing q is 400", func(t *testing.T) {
		rec := ts.do(t, http.MethodGet, "/api/v1/data/tickers", nil, true)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Equal(t, CodeValidation, errCode(t, rec))
	})
	t.Run("blank q is 400", func(t *testing.T) {
		rec := ts.do(t, http.MethodGet, "/api/v1/data/tickers?q=%20", nil, true)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})
	t.Run("bad limit is 400", func(t *testing.T) {
		rec := ts.do(t, http.MethodGet, "/api/v1/data/tickers?q=aapl&limit=0", nil, true)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})
	t.Run("limit over max is 400", func(t *testing.T) {
		rec := ts.do(t, http.MethodGet, "/api/v1/data/tickers?q=aapl&limit=99999", nil, true)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})
	t.Run("happy path", func(t *testing.T) {
		rec := ts.do(t, http.MethodGet, "/api/v1/data/tickers?q=aapl", nil, true)
		require.Equal(t, http.StatusOK, rec.Code)
		body := decodeBody(t, rec)
		assert.Equal(t, "aapl", body["query"])
		results := body["results"].([]any)
		require.Len(t, results, 1)
		assert.Equal(t, "AAPL", results[0].(map[string]any)["ticker"])
	})
	t.Run("db error is 500", func(t *testing.T) {
		ts.data.searchErr = assertErr("nope")
		rec := ts.do(t, http.MethodGet, "/api/v1/data/tickers?q=aapl", nil, true)
		assert.Equal(t, http.StatusInternalServerError, rec.Code)
	})
}

// ---------------------------------------------------------------------------
// GET /api/v1/data/sync-runs
// ---------------------------------------------------------------------------

func TestSyncRuns(t *testing.T) {
	ts := newTestServer(t)
	ts.data.watermarks = []SyncWatermark{{Dataset: "SEP", RowCount: 100, UpdatedAt: fixedNow}}
	ts.data.runs = []SyncRun{{ID: 1, Dataset: "SEP", Kind: "api", Status: "succeeded", StartedAt: fixedNow}}

	t.Run("happy path", func(t *testing.T) {
		rec := ts.do(t, http.MethodGet, "/api/v1/data/sync-runs", nil, true)
		require.Equal(t, http.StatusOK, rec.Code)
		body := decodeBody(t, rec)
		assert.Len(t, body["datasets"], 1)
		assert.Len(t, body["runs"], 1)
	})
	t.Run("unknown dataset is 400", func(t *testing.T) {
		rec := ts.do(t, http.MethodGet, "/api/v1/data/sync-runs?dataset=BOGUS", nil, true)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Equal(t, CodeValidation, errCode(t, rec))
	})
	t.Run("valid dataset filter", func(t *testing.T) {
		rec := ts.do(t, http.MethodGet, "/api/v1/data/sync-runs?dataset=sep", nil, true)
		assert.Equal(t, http.StatusOK, rec.Code)
	})
}

// ---------------------------------------------------------------------------
// POST /api/v1/data/refresh
// ---------------------------------------------------------------------------

func TestDataRefresh(t *testing.T) {
	t.Run("missing source is 400", func(t *testing.T) {
		ts := newTestServer(t)
		rec := ts.do(t, http.MethodPost, "/api/v1/data/refresh", strings.NewReader(`{}`), true)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Equal(t, CodeValidation, errCode(t, rec))
	})
	t.Run("unknown source is 400", func(t *testing.T) {
		ts := newTestServer(t)
		rec := ts.do(t, http.MethodPost, "/api/v1/data/refresh", strings.NewReader(`{"source":"ftp"}`), true)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})
	t.Run("unknown table is 400", func(t *testing.T) {
		ts := newTestServer(t)
		rec := ts.do(t, http.MethodPost, "/api/v1/data/refresh", strings.NewReader(`{"source":"api","tables":["BOGUS"]}`), true)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})
	t.Run("invalid since is 400", func(t *testing.T) {
		ts := newTestServer(t)
		rec := ts.do(t, http.MethodPost, "/api/v1/data/refresh", strings.NewReader(`{"source":"api","since":"06/01/2024"}`), true)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})
	t.Run("unknown json field is 400", func(t *testing.T) {
		ts := newTestServer(t)
		rec := ts.do(t, http.MethodPost, "/api/v1/data/refresh", strings.NewReader(`{"source":"api","bogus":1}`), true)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})
	t.Run("max_attempts out of range is 400", func(t *testing.T) {
		ts := newTestServer(t)
		rec := ts.do(t, http.MethodPost, "/api/v1/data/refresh", strings.NewReader(`{"source":"api","max_attempts":99}`), true)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})
	t.Run("happy path enqueues a job and returns 202", func(t *testing.T) {
		ts := newTestServer(t)
		body := `{"source":"parquet","tables":["sep"],"tickers":["aapl"],"since":"2024-01-01","actor":"alice"}`
		rec := ts.do(t, http.MethodPost, "/api/v1/data/refresh", strings.NewReader(body), true)
		require.Equal(t, http.StatusAccepted, rec.Code)
		out := decodeBody(t, rec)
		job := out["job"].(map[string]any)
		assert.NotZero(t, job["id"])

		require.Len(t, ts.jobs.enqueued, 1)
		p := ts.jobs.enqueued[0]
		assert.Equal(t, "data.refresh", p.Kind)
		assert.Equal(t, "data.refresh", p.DedupeKey)
		assert.Equal(t, "api:alice", p.Actor) // actor stamped with api: prefix
		payload := p.Payload.(map[string]any)
		assert.Equal(t, []string{"SEP"}, payload["tables"]) // upper-cased
		assert.Equal(t, []string{"AAPL"}, payload["tickers"])
	})
	t.Run("blank ticker entry is 400", func(t *testing.T) {
		ts := newTestServer(t)
		rec := ts.do(t, http.MethodPost, "/api/v1/data/refresh", strings.NewReader(`{"source":"api","tickers":["",""]}`), true)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})
}

// ---------------------------------------------------------------------------
// GET /api/v1/jobs  +  /{id}  +  /{id}/cancel
// ---------------------------------------------------------------------------

func TestJobs(t *testing.T) {
	ts := newTestServer(t)
	running := ts.jobs.add(&jobs.Job{Kind: "data.refresh", Status: jobs.StatusRunning, RunAt: fixedNow, CreatedAt: fixedNow, UpdatedAt: fixedNow})
	queued := ts.jobs.add(&jobs.Job{Kind: "universe.rebuild", Status: jobs.StatusQueued, RunAt: fixedNow, CreatedAt: fixedNow, UpdatedAt: fixedNow})

	t.Run("list all", func(t *testing.T) {
		rec := ts.do(t, http.MethodGet, "/api/v1/jobs", nil, true)
		require.Equal(t, http.StatusOK, rec.Code)
		body := decodeBody(t, rec)
		assert.Len(t, body["jobs"], 2)
	})
	t.Run("list filtered by status", func(t *testing.T) {
		rec := ts.do(t, http.MethodGet, "/api/v1/jobs?status=running", nil, true)
		require.Equal(t, http.StatusOK, rec.Code)
		body := decodeBody(t, rec)
		assert.Len(t, body["jobs"], 1)
	})
	t.Run("bad status is 400", func(t *testing.T) {
		rec := ts.do(t, http.MethodGet, "/api/v1/jobs?status=bogus", nil, true)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})
	t.Run("get one", func(t *testing.T) {
		rec := ts.do(t, http.MethodGet, jobPath(running.ID), nil, true)
		require.Equal(t, http.StatusOK, rec.Code)
		body := decodeBody(t, rec)
		assert.Equal(t, float64(running.ID), body["job"].(map[string]any)["id"])
	})
	t.Run("get unknown id is 404", func(t *testing.T) {
		rec := ts.do(t, http.MethodGet, "/api/v1/jobs/9999", nil, true)
		assert.Equal(t, http.StatusNotFound, rec.Code)
		assert.Equal(t, CodeNotFound, errCode(t, rec))
	})
	t.Run("bad id is 400", func(t *testing.T) {
		rec := ts.do(t, http.MethodGet, "/api/v1/jobs/abc", nil, true)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})
	t.Run("cancel queued job", func(t *testing.T) {
		rec := ts.do(t, http.MethodPost, jobPath(queued.ID)+"/cancel", strings.NewReader(`{"reason":"manual"}`), true)
		require.Equal(t, http.StatusOK, rec.Code)
		body := decodeBody(t, rec)
		assert.Equal(t, string(jobs.CancelDone), body["outcome"])
	})
	t.Run("cancel running job sets cancel_requested", func(t *testing.T) {
		rec := ts.do(t, http.MethodPost, jobPath(running.ID)+"/cancel", nil, true)
		require.Equal(t, http.StatusOK, rec.Code)
		body := decodeBody(t, rec)
		assert.Equal(t, string(jobs.CancelRequested), body["outcome"])
	})
	t.Run("cancel unknown id is 404", func(t *testing.T) {
		rec := ts.do(t, http.MethodPost, "/api/v1/jobs/9999/cancel", nil, true)
		assert.Equal(t, http.StatusNotFound, rec.Code)
	})
}

func TestJobs_ListDBError(t *testing.T) {
	ts := newTestServer(t)
	ts.jobs.listErr = assertErr("db down")
	rec := ts.do(t, http.MethodGet, "/api/v1/jobs", nil, true)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// ---------------------------------------------------------------------------
// GET /api/v1/universe/latest  +  POST /api/v1/universe/rebuild
// ---------------------------------------------------------------------------

func TestUniverseLatest(t *testing.T) {
	t.Run("no snapshot is 404", func(t *testing.T) {
		ts := newTestServer(t)
		ts.uni.err = universe.ErrNoSnapshot
		rec := ts.do(t, http.MethodGet, "/api/v1/universe/latest", nil, true)
		assert.Equal(t, http.StatusNotFound, rec.Code)
		assert.Equal(t, CodeNotFound, errCode(t, rec))
	})
	t.Run("bad kind is 400", func(t *testing.T) {
		ts := newTestServer(t)
		rec := ts.do(t, http.MethodGet, "/api/v1/universe/latest?kind=bogus", nil, true)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})
	t.Run("happy path", func(t *testing.T) {
		ts := newTestServer(t)
		ts.uni.snap = &universe.Snapshot{
			ID: 7, AsOf: d(t, "2024-06-11"), Kind: universe.KindEOD,
			Tickers: []string{"AAPL"}, CreatedAt: fixedNow,
			Members: []universe.Member{{Ticker: "AAPL", Rank: 1, Score: 9.5}},
		}
		rec := ts.do(t, http.MethodGet, "/api/v1/universe/latest?kind=eod", nil, true)
		require.Equal(t, http.StatusOK, rec.Code)
		body := decodeBody(t, rec)
		snap := body["snapshot"].(map[string]any)
		assert.Equal(t, float64(7), snap["id"])
		assert.Equal(t, "2024-06-11", snap["as_of"])
		assert.Len(t, snap["members"], 1)
	})
}

func TestUniverseRebuild(t *testing.T) {
	t.Run("defaults kind to manual and enqueues", func(t *testing.T) {
		ts := newTestServer(t)
		rec := ts.do(t, http.MethodPost, "/api/v1/universe/rebuild", strings.NewReader(`{}`), true)
		require.Equal(t, http.StatusAccepted, rec.Code)
		require.Len(t, ts.jobs.enqueued, 1)
		p := ts.jobs.enqueued[0]
		assert.Equal(t, "universe.rebuild", p.Kind)
		assert.Equal(t, "universe.rebuild", p.DedupeKey)
		assert.Equal(t, universe.KindManual, p.Payload.(map[string]any)["kind"])
	})
	t.Run("explicit limit passed through", func(t *testing.T) {
		ts := newTestServer(t)
		rec := ts.do(t, http.MethodPost, "/api/v1/universe/rebuild", strings.NewReader(`{"kind":"eod","limit":50}`), true)
		require.Equal(t, http.StatusAccepted, rec.Code)
		p := ts.jobs.enqueued[0]
		assert.Equal(t, 50, p.Payload.(map[string]any)["limit"])
	})
	t.Run("bad kind is 400", func(t *testing.T) {
		ts := newTestServer(t)
		rec := ts.do(t, http.MethodPost, "/api/v1/universe/rebuild", strings.NewReader(`{"kind":"bogus"}`), true)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})
	t.Run("negative top_k is 400", func(t *testing.T) {
		ts := newTestServer(t)
		rec := ts.do(t, http.MethodPost, "/api/v1/universe/rebuild", strings.NewReader(`{"top_k":-1}`), true)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})
}

// ---------------------------------------------------------------------------
// panic recovery
// ---------------------------------------------------------------------------

func TestRecoverer(t *testing.T) {
	ts := newTestServer(t)
	ts.jobs.enqueueFn = func(_ context.Context, _ jobs.EnqueueParams) (*jobs.Job, bool, error) {
		panic("boom in handler")
	}
	rec := ts.do(t, http.MethodPost, "/api/v1/universe/rebuild", strings.NewReader(`{}`), true)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Equal(t, CodeInternal, errCode(t, rec))
	assert.NotContains(t, rec.Body.String(), "boom in handler")
}
