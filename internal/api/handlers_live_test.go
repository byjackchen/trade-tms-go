package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/commands"
	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
)

// stubLiveReader is an in-memory LiveReader.
type stubLiveReader struct {
	session *LiveSession
	intents []LiveIntent
	health  *LiveHealth
	watch   []string
}

func (s *stubLiveReader) LatestSession(context.Context) (*LiveSession, error) { return s.session, nil }
func (s *stubLiveReader) RecentIntents(_ context.Context, strategyID string, limit int) ([]LiveIntent, error) {
	var out []LiveIntent
	for _, it := range s.intents {
		if strategyID == "" || it.StrategyID == strategyID {
			out = append(out, it)
		}
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}
func (s *stubLiveReader) LatestHealth(context.Context) (*LiveHealth, error) { return s.health, nil }
func (s *stubLiveReader) Watchlist(context.Context) ([]string, error)       { return s.watch, nil }

// stubEnqueuer records enqueued commands and can gate on confirmation.
type stubEnqueuer struct {
	enqueued []commands.EnqueueParams
	nextID   int64
}

func (e *stubEnqueuer) Enqueue(_ context.Context, p commands.EnqueueParams) (int64, error) {
	if commands.RequiresConfirmation(p.Name, p.Args.Mode) && strings.TrimSpace(p.Args.ConfirmToken) == "" {
		return 0, commands.ErrConfirmationRequired
	}
	e.enqueued = append(e.enqueued, p)
	e.nextID++
	return e.nextID, nil
}

func newLiveTestServer(t *testing.T, live LiveReader, enq CommandEnqueuer) *testServer {
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
		PingPG:      pingOK,
		PingRedis:   pingOK,
		Live:        live,
		Commands:    enq,
		Now:         func() time.Time { return fixedNow },
	})
	require.NoError(t, err)
	return &testServer{srv: srv}
}

func TestLiveSessionEndpoint(t *testing.T) {
	live := &stubLiveReader{session: &LiveSession{
		ID: 3, TraderID: "SIGNAL-001", Mode: "signal", Status: "RUNNING",
		StartedAt: fixedNow, Config: json.RawMessage(`{}`),
		Halt: &LiveHalt{Kind: "manual", Reason: "stop", TriggeredAt: fixedNow},
	}}
	ts := newLiveTestServer(t, live, &stubEnqueuer{})

	rec := ts.do(t, http.MethodGet, "/api/v1/live/session", nil, true)
	require.Equal(t, http.StatusOK, rec.Code)
	var got LiveSession
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "SIGNAL-001", got.TraderID)
	assert.Equal(t, "signal", got.Mode)
	require.NotNil(t, got.Halt)
	assert.Equal(t, "manual", got.Halt.Kind)
}

func TestLiveIntentsEndpoint(t *testing.T) {
	live := &stubLiveReader{intents: []LiveIntent{
		{StrategyID: "sepa", Symbol: "AAPL", State: "buy", Strength: 75, Generation: 1,
			Intent: json.RawMessage(`{"symbol":"AAPL"}`), TS: fixedNow},
		{StrategyID: "pairs", Symbol: "KO", State: "hold", Intent: json.RawMessage(`{}`), TS: fixedNow},
	}}
	ts := newLiveTestServer(t, live, &stubEnqueuer{})

	rec := ts.do(t, http.MethodGet, "/api/v1/live/intents?strategy_id=sepa", nil, true)
	require.Equal(t, http.StatusOK, rec.Code)
	var body struct {
		Intents []LiveIntent `json:"intents"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Intents, 1)
	assert.Equal(t, "AAPL", body.Intents[0].Symbol)
}

func TestLiveHealthEndpoint(t *testing.T) {
	ts := newLiveTestServer(t, &stubLiveReader{health: &LiveHealth{TS: fixedNow}}, &stubEnqueuer{})
	rec := ts.do(t, http.MethodGet, "/api/v1/live/health", nil, true)
	require.Equal(t, http.StatusOK, rec.Code)

	// No health -> 503.
	ts2 := newLiveTestServer(t, &stubLiveReader{}, &stubEnqueuer{})
	rec2 := ts2.do(t, http.MethodGet, "/api/v1/live/health", nil, true)
	assert.Equal(t, http.StatusServiceUnavailable, rec2.Code)
}

func TestWatchlistEndpoint(t *testing.T) {
	ts := newLiveTestServer(t, &stubLiveReader{watch: []string{"AAPL", "MSFT"}}, &stubEnqueuer{})
	rec := ts.do(t, http.MethodGet, "/api/v1/watchlist", nil, true)
	require.Equal(t, http.StatusOK, rec.Code)
	var body struct {
		Symbols []string `json:"symbols"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, []string{"AAPL", "MSFT"}, body.Symbols)
}

func TestLiveCommandEnqueue(t *testing.T) {
	enq := &stubEnqueuer{}
	ts := newLiveTestServer(t, &stubLiveReader{}, enq)

	// halt is accepted (202).
	rec := ts.do(t, http.MethodPost, "/api/v1/live/commands",
		strings.NewReader(`{"name":"halt","reason":"stop"}`), true)
	require.Equal(t, http.StatusAccepted, rec.Code)
	require.Len(t, enq.enqueued, 1)
	assert.Equal(t, commands.NameHalt, enq.enqueued[0].Name)

	// set_mode -> live without a token is 412.
	rec = ts.do(t, http.MethodPost, "/api/v1/live/commands",
		strings.NewReader(`{"name":"set_mode","mode":"live"}`), true)
	assert.Equal(t, http.StatusPreconditionFailed, rec.Code)

	// set_mode -> live WITH a token is accepted.
	rec = ts.do(t, http.MethodPost, "/api/v1/live/commands",
		strings.NewReader(`{"name":"set_mode","mode":"live","confirm_token":"ok"}`), true)
	assert.Equal(t, http.StatusAccepted, rec.Code)

	// unknown command -> 400.
	rec = ts.do(t, http.MethodPost, "/api/v1/live/commands",
		strings.NewReader(`{"name":"frobnicate"}`), true)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestLiveEndpointsUnconfigured(t *testing.T) {
	// nil live reader + nil enqueuer -> 503 on every live route.
	ts := newLiveTestServer(t, nil, nil)
	for _, path := range []string{"/api/v1/live/session", "/api/v1/live/intents", "/api/v1/live/health", "/api/v1/watchlist"} {
		rec := ts.do(t, http.MethodGet, path, nil, true)
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code, path)
	}
	rec := ts.do(t, http.MethodPost, "/api/v1/live/commands", strings.NewReader(`{"name":"halt"}`), true)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}
