package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/livetrade"
)

// stubBrokerSync is a controllable BrokerSync for the /trade/sync + /trade/status
// endpoint tests.
type stubBrokerSync struct {
	live             bool
	syncRep          livetrade.SyncReport
	syncErr          error
	lastSyncOperator string
}

func (s *stubBrokerSync) SyncFromBroker(_ context.Context, operator string) (livetrade.SyncReport, error) {
	s.lastSyncOperator = operator
	return s.syncRep, s.syncErr
}
func (s *stubBrokerSync) IsLive() bool { return s.live }

func newBrokerSyncTestServer(t *testing.T, bs BrokerSync) *Server {
	t.Helper()
	cal, err := calendar.NewNYSE()
	require.NoError(t, err)
	srv, err := NewServer(Deps{
		Log:        zerolog.Nop(),
		Token:      testToken,
		Jobs:       newStubJobQueue(),
		Data:       &stubDataStore{barDates: map[string][]calendar.Date{}, tickers: map[string]bool{}},
		Universe:   &stubUniverseReader{},
		Runs:       &stubRunsReader{},
		Strategies: NewStrategyReader(nil, ""),
		Calendar:   cal,
		PingPG:     pingOK,
		PingRedis:  pingOK,
		BrokerSync: bs,
		Now:        func() time.Time { return fixedNow },
	})
	require.NoError(t, err)
	return srv
}

func syncReq(t *testing.T, srv *Server, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, target, http.NoBody)
	}
	r.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, r)
	return rec
}

func TestTradeSyncUnavailableWhenNoDesk(t *testing.T) {
	srv := newBrokerSyncTestServer(t, nil)
	rec := syncReq(t, srv, http.MethodPost, "/api/v1/trade/sync", "")
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestTradeSyncOK(t *testing.T) {
	stub := &stubBrokerSync{syncRep: livetrade.SyncReport{
		PositionsObserved: 2, OrdersObserved: 3, FillsObserved: 5, Reflected: 1,
	}}
	srv := newBrokerSyncTestServer(t, stub)
	rec := syncReq(t, srv, http.MethodPost, "/api/v1/trade/sync", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotEmpty(t, stub.lastSyncOperator, "the sync must carry an operator for the audit")
	body := rec.Body.String()
	assert.Contains(t, body, `"synced"`)
	assert.Contains(t, body, `"positions_observed":2`)
	assert.Contains(t, body, `"reflected":1`)
	assert.Contains(t, body, `"read_only":true`)
}

func TestTradeSyncRequiresAuth(t *testing.T) {
	srv := newBrokerSyncTestServer(t, &stubBrokerSync{})
	r := httptest.NewRequest(http.MethodPost, "/api/v1/trade/sync", http.NoBody)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, r) // no bearer token
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestTradeStatusUnavailableWhenNoDesk(t *testing.T) {
	srv := newBrokerSyncTestServer(t, nil)
	rec := syncReq(t, srv, http.MethodGet, "/api/v1/trade/status", "")
	// The sync-availability probe is 503 when no session is connected — the e2e
	// skip-guard reads THIS (not the always-200 live-account reader), so the
	// broker-sync specs skip cleanly rather than hard-fail on a 503 POST.
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestTradeStatusConnectedPaper(t *testing.T) {
	srv := newBrokerSyncTestServer(t, &stubBrokerSync{live: false})
	rec := syncReq(t, srv, http.MethodGet, "/api/v1/trade/status", "")
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, `"connected":true`)
	assert.Contains(t, body, `"mode":"paper"`)
	assert.Contains(t, body, `"live":false`)
}

func TestTradeStatusConnectedLive(t *testing.T) {
	srv := newBrokerSyncTestServer(t, &stubBrokerSync{live: true})
	rec := syncReq(t, srv, http.MethodGet, "/api/v1/trade/status", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"mode":"live"`)
	assert.Contains(t, rec.Body.String(), `"live":true`)
}
