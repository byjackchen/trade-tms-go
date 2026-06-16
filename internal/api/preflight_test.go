package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
)

// stubPreflight records the params it was called with and returns a canned
// report.
type stubPreflight struct {
	got    PreflightParams
	report PreflightReport
}

func (s *stubPreflight) RunPreflight(_ context.Context, p PreflightParams) PreflightReport {
	s.got = p
	return s.report
}

func newPreflightServer(t *testing.T, pf PreflightRunner) *Server {
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
		Preflight:  pf,
		Now:        func() time.Time { return fixedNow },
	})
	require.NoError(t, err)
	return srv
}

func getPreflight(t *testing.T, srv *Server, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	return rec
}

func TestHandleLivePreflight_NotConfigured(t *testing.T) {
	srv := newPreflightServer(t, nil)
	rec := getPreflight(t, srv, "/api/v1/trade/preflight")
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestHandleLivePreflight_OK(t *testing.T) {
	pf := &stubPreflight{report: PreflightReport{
		Mode: "paper", Strategy: "multi", OK: true,
		Checks: []PreflightResult{{Check: "DATA_CURRENT", Status: "pass", Severity: "blocker", Detail: "fresh"}},
	}}
	srv := newPreflightServer(t, pf)
	rec := getPreflight(t, srv, "/api/v1/trade/preflight?mode=paper&strategy=multi&tickers=AAPL,MSFT&check_opend=1&max_stale_days=2")
	require.Equal(t, http.StatusOK, rec.Code)

	var body PreflightReport
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.True(t, body.OK)
	require.Equal(t, "paper", body.Mode)
	require.Len(t, body.Checks, 1)

	// The handler parsed the query into params.
	require.Equal(t, "paper", pf.got.Mode)
	require.Equal(t, "multi", pf.got.Strategy)
	require.Equal(t, []string{"AAPL", "MSFT"}, pf.got.Tickers)
	require.True(t, pf.got.CheckOpenD)
	require.Equal(t, 2, pf.got.MaxStaleTradingDays)
}

func TestHandleLivePreflight_Defaults(t *testing.T) {
	pf := &stubPreflight{report: PreflightReport{OK: false}}
	srv := newPreflightServer(t, pf)
	rec := getPreflight(t, srv, "/api/v1/trade/preflight")
	require.Equal(t, http.StatusOK, rec.Code) // failing preflight is still HTTP 200
	require.Equal(t, "signal", pf.got.Mode)
	require.Equal(t, "multi", pf.got.Strategy)
	require.Equal(t, 1, pf.got.MaxStaleTradingDays)
	require.False(t, pf.got.CheckOpenD)
}

func TestHandleLivePreflight_BadParams(t *testing.T) {
	pf := &stubPreflight{}
	srv := newPreflightServer(t, pf)
	for _, target := range []string{
		"/api/v1/trade/preflight?mode=bogus",
		"/api/v1/trade/preflight?strategy=bogus",
		"/api/v1/trade/preflight?max_stale_days=-3",
		"/api/v1/trade/preflight?max_stale_days=abc",
	} {
		rec := getPreflight(t, srv, target)
		require.Equal(t, http.StatusBadRequest, rec.Code, "target %s", target)
	}
}

func TestHandleLivePreflight_RequiresAuth(t *testing.T) {
	srv := newPreflightServer(t, &stubPreflight{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/trade/preflight", nil)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}
