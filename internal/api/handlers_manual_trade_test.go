package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	moo "github.com/byjackchen/trade-tms-go/internal/broker/moomoo"
	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/livetrade"
)

// stubManual is a controllable ManualTrader for the /trade/* endpoint tests.
type stubManual struct {
	live             bool
	placeErr         error
	placeRes         livetrade.ManualOrderResult
	cancelErr        error
	closeErr         error
	closeRes         livetrade.ManualOrderResult
	syncRep          livetrade.SyncReport
	syncErr          error
	lastPlace        livetrade.ManualOrderRequest
	lastCancel       string
	lastSyncOperator string
	lastClose        struct {
		symbol  string
		qty     domain.Qty
		idemKey string
	}
}

func (s *stubManual) PlaceManualOrder(_ context.Context, r livetrade.ManualOrderRequest) (livetrade.ManualOrderResult, error) {
	s.lastPlace = r
	return s.placeRes, s.placeErr
}
func (s *stubManual) CancelManualOrder(_ context.Context, _, coid string) error {
	s.lastCancel = coid
	return s.cancelErr
}
func (s *stubManual) CloseManualPosition(_ context.Context, _, symbol string, qty domain.Qty, _ string, idemKey string) (livetrade.ManualOrderResult, error) {
	s.lastClose.symbol = symbol
	s.lastClose.qty = qty
	s.lastClose.idemKey = idemKey
	return s.closeRes, s.closeErr
}
func (s *stubManual) SyncFromBroker(_ context.Context, operator string) (livetrade.SyncReport, error) {
	s.lastSyncOperator = operator
	return s.syncRep, s.syncErr
}
func (s *stubManual) IsLive() bool { return s.live }

func newManualTestServer(t *testing.T, manual ManualTrader) *Server {
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
		Manual:     manual,
		Now:        func() time.Time { return fixedNow },
	})
	require.NoError(t, err)
	return srv
}

func manualReq(t *testing.T, srv *Server, method, target, body string) *httptest.ResponseRecorder {
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

func TestTradeOrderUnavailableWhenNoDesk(t *testing.T) {
	srv := newManualTestServer(t, nil)
	rec := manualReq(t, srv, http.MethodPost, "/api/v1/trade/order",
		`{"idempotency_key":"k","symbol":"AAPL","side":"BUY","qty":1}`)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestTradeOrderPlaced(t *testing.T) {
	stub := &stubManual{placeRes: livetrade.ManualOrderResult{ClientOrderID: "MANUAL-PAPER-k", Submitted: true}}
	srv := newManualTestServer(t, stub)
	rec := manualReq(t, srv, http.MethodPost, "/api/v1/trade/order",
		`{"idempotency_key":"k","symbol":"aapl","side":"buy","qty":10}`)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "MANUAL-PAPER-k")
	// Symbol/side normalized to upper-case before reaching the desk.
	assert.Equal(t, "AAPL", stub.lastPlace.Symbol)
	assert.Equal(t, domain.OrderSideBuy, stub.lastPlace.Side)
	assert.Equal(t, domain.Qty(10), stub.lastPlace.Qty)
}

func TestTradeOrderConfirmationRequired412(t *testing.T) {
	stub := &stubManual{live: true, placeErr: livetrade.ErrConfirmationRequired}
	srv := newManualTestServer(t, stub)
	rec := manualReq(t, srv, http.MethodPost, "/api/v1/trade/order",
		`{"idempotency_key":"k","symbol":"AAPL","side":"BUY","qty":1}`)
	require.Equal(t, http.StatusPreconditionFailed, rec.Code)
	assert.Contains(t, rec.Body.String(), "confirmation_required")
}

func TestTradeOrderRiskViolation422(t *testing.T) {
	stub := &stubManual{placeErr: livetrade.ErrRiskViolation}
	srv := newManualTestServer(t, stub)
	rec := manualReq(t, srv, http.MethodPost, "/api/v1/trade/order",
		`{"idempotency_key":"k","symbol":"AAPL","side":"BUY","qty":1,"confirm_token":"x"}`)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Contains(t, rec.Body.String(), "risk_violation")
}

func TestTradeOrderVenueRejected422(t *testing.T) {
	// A broker BUSINESS rejection (insufficient buying power, market closed, unknown
	// symbol) must surface as a clean 422 order_rejected — NOT a 500 with a leaked
	// protocol string (finding 4). No order was placed.
	stub := &stubManual{placeErr: fmt.Errorf("moomoo executor: manual place order X: %w: Trd_PlaceOrder retType=-1 retMsg=\"insufficient buying power\"", moo.ErrOrderRejected)}
	srv := newManualTestServer(t, stub)
	rec := manualReq(t, srv, http.MethodPost, "/api/v1/trade/order",
		`{"idempotency_key":"k","symbol":"AAPL","side":"BUY","qty":100000000,"confirm_token":"x"}`)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Contains(t, rec.Body.String(), "order_rejected")
	assert.Contains(t, rec.Body.String(), "insufficient buying power")
}

func TestTradeOrderBadSide400(t *testing.T) {
	srv := newManualTestServer(t, &stubManual{})
	rec := manualReq(t, srv, http.MethodPost, "/api/v1/trade/order",
		`{"idempotency_key":"k","symbol":"AAPL","side":"HOLD","qty":1}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestTradeOrderLimitRequiresPrice400(t *testing.T) {
	srv := newManualTestServer(t, &stubManual{})
	rec := manualReq(t, srv, http.MethodPost, "/api/v1/trade/order",
		`{"idempotency_key":"k","symbol":"AAPL","side":"BUY","qty":1,"type":"LIMIT"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestTradeCancel(t *testing.T) {
	stub := &stubManual{}
	srv := newManualTestServer(t, stub)
	rec := manualReq(t, srv, http.MethodPost, "/api/v1/trade/order/MANUAL-PAPER-k/cancel", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "MANUAL-PAPER-k", stub.lastCancel)
	assert.Contains(t, rec.Body.String(), "cancel_requested")
}

func TestTradeClose(t *testing.T) {
	stub := &stubManual{closeRes: livetrade.ManualOrderResult{ClientOrderID: "MANUAL-PAPER-close", Submitted: true}}
	srv := newManualTestServer(t, stub)
	rec := manualReq(t, srv, http.MethodPost, "/api/v1/trade/position/aapl/close", `{"confirm_token":"x"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "AAPL", stub.lastClose.symbol)
	assert.Contains(t, rec.Body.String(), "close_submitted")
}

func TestTradeCloseConfirmationRequired412(t *testing.T) {
	stub := &stubManual{live: true, closeErr: livetrade.ErrConfirmationRequired}
	srv := newManualTestServer(t, stub)
	rec := manualReq(t, srv, http.MethodPost, "/api/v1/trade/position/AAPL/close", `{}`)
	assert.Equal(t, http.StatusPreconditionFailed, rec.Code)
}

func TestTradeSyncUnavailableWhenNoDesk(t *testing.T) {
	srv := newManualTestServer(t, nil)
	rec := manualReq(t, srv, http.MethodPost, "/api/v1/trade/sync", "")
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestTradeSyncOK(t *testing.T) {
	stub := &stubManual{syncRep: livetrade.SyncReport{
		PositionsObserved: 2, OrdersObserved: 3, FillsObserved: 5, Reflected: 1,
	}}
	srv := newManualTestServer(t, stub)
	rec := manualReq(t, srv, http.MethodPost, "/api/v1/trade/sync", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotEmpty(t, stub.lastSyncOperator, "the sync must carry an operator for the audit")
	body := rec.Body.String()
	assert.Contains(t, body, `"synced"`)
	assert.Contains(t, body, `"positions_observed":2`)
	assert.Contains(t, body, `"reflected":1`)
	assert.Contains(t, body, `"read_only":true`)
}

func TestTradeSyncRequiresAuth(t *testing.T) {
	srv := newManualTestServer(t, &stubManual{})
	r := httptest.NewRequest(http.MethodPost, "/api/v1/trade/sync", http.NoBody)
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, r) // no bearer token
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestTradeEndpointsRequireAuth(t *testing.T) {
	srv := newManualTestServer(t, &stubManual{})
	r := httptest.NewRequest(http.MethodPost, "/api/v1/trade/order",
		strings.NewReader(`{"idempotency_key":"k","symbol":"AAPL","side":"BUY","qty":1}`))
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, r) // no bearer token
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestTradeStatusUnavailableWhenNoDesk(t *testing.T) {
	srv := newManualTestServer(t, nil)
	rec := manualReq(t, srv, http.MethodGet, "/api/v1/trade/status", "")
	// The desk-availability probe is 503 when no desk is connected — the e2e
	// skip-guard reads THIS (not the always-200 live-account reader), so the
	// manual-desk specs skip cleanly rather than hard-fail on a 503 POST.
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestTradeStatusConnectedPaper(t *testing.T) {
	srv := newManualTestServer(t, &stubManual{live: false})
	rec := manualReq(t, srv, http.MethodGet, "/api/v1/trade/status", "")
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, `"connected":true`)
	assert.Contains(t, body, `"mode":"paper"`)
	assert.Contains(t, body, `"live":false`)
}

func TestTradeStatusConnectedLive(t *testing.T) {
	srv := newManualTestServer(t, &stubManual{live: true})
	rec := manualReq(t, srv, http.MethodGet, "/api/v1/trade/status", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"mode":"live"`)
	assert.Contains(t, rec.Body.String(), `"live":true`)
}

func TestTradeCloseForwardsIdempotencyKey(t *testing.T) {
	stub := &stubManual{closeRes: livetrade.ManualOrderResult{ClientOrderID: "MANUAL-PAPER-x", Submitted: true}}
	srv := newManualTestServer(t, stub)
	rec := manualReq(t, srv, http.MethodPost, "/api/v1/trade/position/AAPL/close",
		`{"qty":0,"confirm_token":"pw","idempotency_key":"close-key-1"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	// The idempotency key reaches the desk so a double-click / retry dedupes the
	// close client-order-id (no real-money oversell).
	assert.Equal(t, "close-key-1", stub.lastClose.idemKey)
}
