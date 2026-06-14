package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
)

// stubSystemReader is an in-memory SystemReader for the GET /api/v1/system
// contract tests.
type stubSystemReader struct {
	queued, running int
	active          int
	barDate         string
	lastSync        *time.Time
	err             error
}

func (s *stubSystemReader) QueueDepth(context.Context) (int, int, error) {
	return s.queued, s.running, s.err
}
func (s *stubSystemReader) ActiveSessions(context.Context) (int, error) {
	return s.active, s.err
}
func (s *stubSystemReader) DataFreshness(context.Context) (string, *time.Time, error) {
	return s.barDate, s.lastSync, s.err
}

// newSystemTestServer builds a server wired with the system reader + live reader
// + custom pings, for the aggregation tests.
func newSystemTestServer(t *testing.T, sys SystemReader, live LiveReader, pgPing, redisPing PingFunc) *testServer {
	t.Helper()
	cal, err := calendar.NewNYSE()
	require.NoError(t, err)
	srv, err := NewServer(Deps{
		Log:         zerolog.Nop(),
		Token:       testToken,
		CORSOrigins: []string{testOrigin},
		Jobs:        newStubJobQueue(),
		Data:        &stubDataStore{barDates: map[string][]calendar.Date{}, tickers: map[string]bool{}},
		Universe:    &stubUniverseReader{},
		Runs:        &stubRunsReader{},
		Strategies:  NewStrategyReader(nil, ""),
		Hyperopt:    &stubHyperoptReader{},
		Promoter:    &stubPromoter{},
		Calendar:    cal,
		PingPG:      pgPing,
		PingRedis:   redisPing,
		Live:        live,
		System:      sys,
		Now:         func() time.Time { return fixedNow },
	})
	require.NoError(t, err)
	return &testServer{srv: srv}
}

// TestSystemEndpointHealthyAggregation: a fully healthy stack with a RUNNING
// session and a fresh health snapshot rolls up to "ok" and surfaces every
// component + metric.
func TestSystemEndpointHealthyAggregation(t *testing.T) {
	sync := fixedNow.Add(-2 * time.Hour)
	sys := &stubSystemReader{queued: 3, running: 1, active: 1, barDate: "2025-06-10", lastSync: &sync}
	live := &stubLiveReader{
		session: &LiveSession{ID: 7, TraderID: "SIGNAL-001", Mode: "signal", Status: "RUNNING", StartedAt: fixedNow},
		// Health one minute old => inside the freshness window => "data flowing".
		health: &LiveHealth{TS: fixedNow.Add(-1 * time.Minute)},
	}
	ts := newSystemTestServer(t, sys, live, pingOK, pingOK)

	rec := ts.do(t, http.MethodGet, "/api/v1/system", nil, true)
	require.Equal(t, http.StatusOK, rec.Code)

	var got SystemResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got), "body: %s", rec.Body.String())

	assert.Equal(t, "ok", got.Status)
	assert.Equal(t, "ok", got.Components["postgres"].Status)
	assert.Equal(t, "ok", got.Components["redis"].Status)
	assert.Equal(t, "ok", got.Components["data"].Status)
	assert.Equal(t, "ok", got.Components["moomoo_feed"].Status, "running+fresh health => feed flowing")
	assert.Equal(t, "data flowing", got.Components["moomoo_feed"].Detail)

	assert.Equal(t, 3, got.Metrics.JobsQueued)
	assert.Equal(t, 1, got.Metrics.JobsRunning)
	assert.Equal(t, 1, got.Metrics.ActiveSessions)
	assert.Equal(t, "2025-06-10", got.Metrics.LatestBarDate)
	require.NotNil(t, got.Metrics.LiveSessionID)
	assert.Equal(t, int64(7), *got.Metrics.LiveSessionID)
	assert.Equal(t, "signal", got.Metrics.LiveMode)
	require.NotNil(t, got.Metrics.HealthAgeSecond)
	assert.InDelta(t, 60.0, *got.Metrics.HealthAgeSecond, 0.5)
}

// TestSystemEndpointPostgresDownIsDown: Postgres unreachable rolls the whole
// system to "down" (the truth store is the only fatal dependency).
func TestSystemEndpointPostgresDownIsDown(t *testing.T) {
	sys := &stubSystemReader{}
	ts := newSystemTestServer(t, sys, &stubLiveReader{}, pingErr, pingOK)

	rec := ts.do(t, http.MethodGet, "/api/v1/system", nil, true)
	require.Equal(t, http.StatusOK, rec.Code, "endpoint is always 200; degradation is in the body")

	var got SystemResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "down", got.Status)
	assert.Equal(t, "down", got.Components["postgres"].Status)
}

// TestSystemEndpointRedisDownIsDegraded: Redis down (non-fatal transport) rolls
// to "degraded", not "down".
func TestSystemEndpointRedisDownIsDegraded(t *testing.T) {
	sys := &stubSystemReader{}
	ts := newSystemTestServer(t, sys, &stubLiveReader{}, pingOK, pingErr)

	rec := ts.do(t, http.MethodGet, "/api/v1/system", nil, true)
	require.Equal(t, http.StatusOK, rec.Code)

	var got SystemResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "degraded", got.Status)
	assert.Equal(t, "down", got.Components["redis"].Status)
	assert.Equal(t, "ok", got.Components["postgres"].Status)
}

// TestSystemEndpointStaleFeed: a RUNNING session whose last health snapshot is
// older than the freshness window reports the feed degraded (bars stale) but the
// session itself is still counted active.
func TestSystemEndpointStaleFeed(t *testing.T) {
	sys := &stubSystemReader{active: 1}
	live := &stubLiveReader{
		session: &LiveSession{ID: 9, Mode: "paper", Status: "RUNNING", StartedAt: fixedNow},
		health:  &LiveHealth{TS: fixedNow.Add(-30 * time.Minute)}, // outside 5m window
	}
	ts := newSystemTestServer(t, sys, live, pingOK, pingOK)

	rec := ts.do(t, http.MethodGet, "/api/v1/system", nil, true)
	var got SystemResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "degraded", got.Status)
	assert.Equal(t, "degraded", got.Components["moomoo_feed"].Status)
	assert.Equal(t, "paper", got.Metrics.LiveMode)
}

// TestSystemEndpointNoSession: with no live session the feed is "idle" and the
// rollup stays "ok" (idle is not a degradation).
func TestSystemEndpointNoSession(t *testing.T) {
	sys := &stubSystemReader{barDate: "2025-06-10"}
	ts := newSystemTestServer(t, sys, &stubLiveReader{}, pingOK, pingOK)

	rec := ts.do(t, http.MethodGet, "/api/v1/system", nil, true)
	var got SystemResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "ok", got.Status)
	assert.Equal(t, "idle", got.Components["moomoo_feed"].Status)
	assert.Equal(t, "idle", got.Components["sessions"].Status)
}

// TestSystemEndpointRequiresAuth: the endpoint is behind the bearer guard.
func TestSystemEndpointRequiresAuth(t *testing.T) {
	ts := newSystemTestServer(t, &stubSystemReader{}, &stubLiveReader{}, pingOK, pingOK)
	rec := ts.do(t, http.MethodGet, "/api/v1/system", nil, false)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}
