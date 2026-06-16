package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
)

// newProxyTestServer builds an API server whose /api/v1/trade/* is reverse-proxied
// to the given upstream base URL (the live node's manual listener stand-in).
func newProxyTestServer(t *testing.T, upstream string) *Server {
	t.Helper()
	cal, err := calendar.NewNYSE()
	require.NoError(t, err)
	var proxy *ManualTradeProxy
	if upstream != "" {
		proxy, err = NewManualTradeProxy(upstream, zerolog.Nop())
		require.NoError(t, err)
	}
	srv, err := NewServer(Deps{
		Log:         zerolog.Nop(),
		Token:       testToken,
		Jobs:        newStubJobQueue(),
		Data:        &stubDataStore{barDates: map[string][]calendar.Date{}, tickers: map[string]bool{}},
		Universe:    &stubUniverseReader{},
		Runs:        &stubRunsReader{},
		Strategies:  NewStrategyReader(nil, ""),
		Calendar:    cal,
		PingPG:      pingOK,
		PingRedis:   pingOK,
		ManualProxy: proxy,
		Now:         func() time.Time { return fixedNow },
	})
	require.NoError(t, err)
	return srv
}

// TestManualTradeProxyForwards proves the API reverse-proxies /api/v1/trade/* onto
// the live node's manual listener verbatim: the upstream sees the same path + body +
// bearer token, and its status/body pass through (the shipped compose topology, so
// the UI + e2e hit one host while the broker-connected live node executes).
func TestManualTradeProxyForwards(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"client_order_id":"MANUAL-PAPER-k","submitted":true,"status":"submitted"}`))
	}))
	defer upstream.Close()

	srv := newProxyTestServer(t, upstream.URL)
	rec := manualReq(t, srv, http.MethodPost, "/api/v1/trade/order",
		`{"idempotency_key":"k","symbol":"AAPL","side":"BUY","qty":1}`)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "MANUAL-PAPER-k")
	assert.Equal(t, "/api/v1/trade/order", gotPath, "the upstream sees the same path")
	assert.Equal(t, "Bearer "+testToken, gotAuth, "the bearer token is forwarded for re-auth")
	assert.Contains(t, gotBody, `"symbol":"AAPL"`, "the body is forwarded verbatim")
}

// TestManualTradeProxyStatusProxied proves GET /trade/status is proxied too (the
// desk-availability probe lands on the broker-connected node).
func TestManualTradeProxyStatusProxied(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/trade/status", r.URL.Path)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"connected":true,"mode":"paper","live":false}`))
	}))
	defer upstream.Close()

	srv := newProxyTestServer(t, upstream.URL)
	rec := manualReq(t, srv, http.MethodGet, "/api/v1/trade/status", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"mode":"paper"`)
}

// TestManualTradeProxyUpstreamDown proves an unreachable live-node manual listener
// surfaces as 503 (no desk connected), NOT 502 — so the e2e skip-guard + the UI
// treat it identically to the no-upstream case (the desk is simply not connected).
func TestManualTradeProxyUpstreamDown(t *testing.T) {
	// A URL that is syntactically valid but refuses connections.
	srv := newProxyTestServer(t, "http://127.0.0.1:1") // port 1: connection refused
	rec := manualReq(t, srv, http.MethodPost, "/api/v1/trade/order",
		`{"idempotency_key":"k","symbol":"AAPL","side":"BUY","qty":1}`)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

// TestManualTradeProxyRejectsRelativeURL proves the constructor refuses a
// non-absolute upstream (a misconfiguration is a startup error, not a silent
// degraded mode).
func TestManualTradeProxyRejectsRelativeURL(t *testing.T) {
	_, err := NewManualTradeProxy("tmsgo-live:18091", zerolog.Nop())
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "absolute"))
}
