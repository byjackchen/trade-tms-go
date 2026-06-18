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

// stubTradeReader is an in-memory TradeReader.
type stubTradeReader struct {
	session  *TradeSession
	intents  []TradeSignal
	health   *TradeHealth
	watch    []string
	accounts []TradeAccountInfo
}

func (s *stubTradeReader) LatestSession(context.Context) (*TradeSession, error) {
	return s.session, nil
}
func (s *stubTradeReader) RecentSignals(_ context.Context, strategyID string, limit int) ([]TradeSignal, error) {
	var out []TradeSignal
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
func (s *stubTradeReader) LatestHealth(context.Context) (*TradeHealth, error) { return s.health, nil }
func (s *stubTradeReader) Watchlist(context.Context) ([]string, error)        { return s.watch, nil }
func (s *stubTradeReader) LatestSignalsBySymbol(_ context.Context, _ int) ([]TradeSignal, error) {
	return s.intents, nil
}
func (s *stubTradeReader) ListAccounts(context.Context) ([]TradeAccountInfo, error) {
	return s.accounts, nil
}

// stubTradeTradingReader is an in-memory TradeReader + TradeTradingReader that
// records the account filter the handlers pass through, so the account_id query
// param can be asserted end-to-end.
type stubTradeTradingReader struct {
	stubTradeReader
	orders    []TradeOrder
	fills     []TradeFill
	positions []TradePosition

	lastOrdersAcct    string
	lastFillsAcct     string
	lastPositionsAcct string
}

func (s *stubTradeTradingReader) RecentOrders(ctx context.Context, symbol string, limit int) ([]TradeOrder, error) {
	return s.RecentOrdersFor(ctx, symbol, "", limit)
}
func (s *stubTradeTradingReader) RecentOrdersFor(_ context.Context, _, accountID string, _ int) ([]TradeOrder, error) {
	s.lastOrdersAcct = accountID
	return s.orders, nil
}
func (s *stubTradeTradingReader) RecentFills(ctx context.Context, symbol string, limit int) ([]TradeFill, error) {
	return s.RecentFillsFor(ctx, symbol, "", limit)
}
func (s *stubTradeTradingReader) RecentFillsFor(_ context.Context, _, accountID string, _ int) ([]TradeFill, error) {
	s.lastFillsAcct = accountID
	return s.fills, nil
}
func (s *stubTradeTradingReader) OpenPositions(ctx context.Context) ([]TradePosition, error) {
	return s.OpenPositionsFor(ctx, "")
}
func (s *stubTradeTradingReader) OpenPositionsFor(_ context.Context, accountID string) ([]TradePosition, error) {
	s.lastPositionsAcct = accountID
	return s.positions, nil
}
func (s *stubTradeTradingReader) SessionRealizedPnL(context.Context) (float64, error) { return 0, nil }
func (s *stubTradeTradingReader) LatestReconciliation(context.Context) (*TradeReconciliation, error) {
	return nil, nil
}

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

func newTradeTestServer(t *testing.T, trade TradeReader, enq CommandEnqueuer) *testServer {
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
		Trade:       trade,
		Commands:    enq,
		Now:         func() time.Time { return fixedNow },
	})
	require.NoError(t, err)
	return &testServer{srv: srv}
}

func TestTradeSessionEndpoint(t *testing.T) {
	trade := &stubTradeReader{session: &TradeSession{
		ID: 3, TraderID: "SIGNAL-001", ExecPolicy: "signal", Status: "RUNNING",
		StartedAt: fixedNow, Config: json.RawMessage(`{}`),
		Halt: &TradeHalt{Kind: "manual", Reason: "stop", TriggeredAt: fixedNow},
	}}
	ts := newTradeTestServer(t, trade, &stubEnqueuer{})

	rec := ts.do(t, http.MethodGet, "/api/v1/trade/session", nil, true)
	require.Equal(t, http.StatusOK, rec.Code)
	var got TradeSession
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "SIGNAL-001", got.TraderID)
	assert.Equal(t, "signal", got.ExecPolicy)
	require.NotNil(t, got.Halt)
	assert.Equal(t, "manual", got.Halt.Kind)
}

func TestTradeSignalsEndpoint(t *testing.T) {
	trade := &stubTradeReader{intents: []TradeSignal{
		{StrategyID: "sepa", Symbol: "AAPL", State: "buy", Strength: 75, Generation: 1,
			Signal: json.RawMessage(`{"symbol":"AAPL"}`), TS: fixedNow},
		{StrategyID: "pairs", Symbol: "KO", State: "hold", Signal: json.RawMessage(`{}`), TS: fixedNow},
	}}
	ts := newTradeTestServer(t, trade, &stubEnqueuer{})

	rec := ts.do(t, http.MethodGet, "/api/v1/trade/signals?strategy_id=sepa", nil, true)
	require.Equal(t, http.StatusOK, rec.Code)
	var body struct {
		Signals []TradeSignal `json:"signals"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Signals, 1)
	assert.Equal(t, "AAPL", body.Signals[0].Symbol)
}

func TestTradeHealthEndpoint(t *testing.T) {
	ts := newTradeTestServer(t, &stubTradeReader{health: &TradeHealth{TS: fixedNow}}, &stubEnqueuer{})
	rec := ts.do(t, http.MethodGet, "/api/v1/trade/health", nil, true)
	require.Equal(t, http.StatusOK, rec.Code)

	// No health -> 503.
	ts2 := newTradeTestServer(t, &stubTradeReader{}, &stubEnqueuer{})
	rec2 := ts2.do(t, http.MethodGet, "/api/v1/trade/health", nil, true)
	assert.Equal(t, http.StatusServiceUnavailable, rec2.Code)
}

func TestWatchlistEndpoint(t *testing.T) {
	ts := newTradeTestServer(t, &stubTradeReader{watch: []string{"AAPL", "MSFT"}}, &stubEnqueuer{})
	rec := ts.do(t, http.MethodGet, "/api/v1/watchlist", nil, true)
	require.Equal(t, http.StatusOK, rec.Code)
	var body struct {
		Symbols []string `json:"symbols"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, []string{"AAPL", "MSFT"}, body.Symbols)
}

func TestTradeAccountsEndpoint(t *testing.T) {
	trade := &stubTradeReader{accounts: []TradeAccountInfo{
		{ID: "moomoo:real:123", Venue: "moomoo", Env: "real", BrokerAccID: 123, Label: "live"},
		{ID: "simu:signal", Venue: "simu", Env: "simu", BrokerAccID: 0, Label: ""},
	}}
	ts := newTradeTestServer(t, trade, &stubEnqueuer{})

	rec := ts.do(t, http.MethodGet, "/api/v1/trade/accounts", nil, true)
	require.Equal(t, http.StatusOK, rec.Code)
	var body struct {
		Accounts []TradeAccountInfo `json:"accounts"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Accounts, 2)
	assert.Equal(t, "moomoo:real:123", body.Accounts[0].ID)
	assert.Equal(t, int64(123), body.Accounts[0].BrokerAccID)
	assert.Equal(t, "real", body.Accounts[0].Env)

	// nil reader -> 503.
	ts2 := newTradeTestServer(t, nil, nil)
	rec2 := ts2.do(t, http.MethodGet, "/api/v1/trade/accounts", nil, true)
	assert.Equal(t, http.StatusServiceUnavailable, rec2.Code)
}

func TestTradeAccountFilter(t *testing.T) {
	tr := &stubTradeTradingReader{}
	ts := newTradeTestServer(t, tr, &stubEnqueuer{})

	// account_id present -> threaded into the reads.
	rec := ts.do(t, http.MethodGet, "/api/v1/trade/orders?account_id=moomoo:real:123", nil, true)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "moomoo:real:123", tr.lastOrdersAcct)

	rec = ts.do(t, http.MethodGet, "/api/v1/trade/fills?account_id=simu:signal", nil, true)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "simu:signal", tr.lastFillsAcct)

	rec = ts.do(t, http.MethodGet, "/api/v1/trade/positions?account_id=moomoo:real:123", nil, true)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "moomoo:real:123", tr.lastPositionsAcct)

	// absent -> empty filter (unchanged behavior / all accounts).
	rec = ts.do(t, http.MethodGet, "/api/v1/trade/positions", nil, true)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "", tr.lastPositionsAcct)
}

func TestTradeCommandEnqueue(t *testing.T) {
	enq := &stubEnqueuer{}
	ts := newTradeTestServer(t, &stubTradeReader{}, enq)

	// halt is accepted (202).
	rec := ts.do(t, http.MethodPost, "/api/v1/trade/commands",
		strings.NewReader(`{"name":"halt","reason":"stop"}`), true)
	require.Equal(t, http.StatusAccepted, rec.Code)
	require.Len(t, enq.enqueued, 1)
	assert.Equal(t, commands.NameHalt, enq.enqueued[0].Name)

	// set_mode -> live (exec_policy=auto + env=real) without a token is 412.
	rec = ts.do(t, http.MethodPost, "/api/v1/trade/commands",
		strings.NewReader(`{"name":"set_mode","exec_policy":"auto","env":"real"}`), true)
	assert.Equal(t, http.StatusPreconditionFailed, rec.Code)

	// set_mode -> live WITH a token is accepted.
	rec = ts.do(t, http.MethodPost, "/api/v1/trade/commands",
		strings.NewReader(`{"name":"set_mode","exec_policy":"auto","env":"real","confirm_token":"ok"}`), true)
	assert.Equal(t, http.StatusAccepted, rec.Code)

	// unknown command -> 400.
	rec = ts.do(t, http.MethodPost, "/api/v1/trade/commands",
		strings.NewReader(`{"name":"frobnicate"}`), true)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestTradeEndpointsUnconfigured(t *testing.T) {
	// nil trade reader + nil enqueuer -> 503 on every trade route.
	ts := newTradeTestServer(t, nil, nil)
	for _, path := range []string{"/api/v1/trade/session", "/api/v1/trade/signals", "/api/v1/trade/health", "/api/v1/watchlist", "/api/v1/trade/accounts"} {
		rec := ts.do(t, http.MethodGet, path, nil, true)
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code, path)
	}
	rec := ts.do(t, http.MethodPost, "/api/v1/trade/commands", strings.NewReader(`{"name":"halt"}`), true)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

// TestLegacyLiveRedirects locks the back-compat aliases: the old /live/* read/control
// paths 308-redirect to their /trade/* equivalents (query string AND method preserved)
// so a not-yet-updated UI keeps working through the rename — including the POST-only
// /commands control surface, which a 301 would have downgraded to GET → 404.
func TestLegacyLiveRedirects(t *testing.T) {
	ts := newTradeTestServer(t, &stubTradeReader{}, &stubEnqueuer{})
	cases := []struct {
		method, path, wantLocation string
	}{
		{http.MethodGet, "/api/v1/live/session", "/api/v1/trade/session"},
		{http.MethodGet, "/api/v1/live/intents?strategy_id=sepa", "/api/v1/trade/signals?strategy_id=sepa"},
		{http.MethodGet, "/api/v1/live/health", "/api/v1/trade/health"},
		{http.MethodGet, "/api/v1/live/preflight", "/api/v1/trade/preflight"},
		{http.MethodGet, "/api/v1/live/orders", "/api/v1/trade/orders"},
		{http.MethodGet, "/api/v1/live/fills", "/api/v1/trade/fills"},
		{http.MethodGet, "/api/v1/live/positions", "/api/v1/trade/positions"},
		{http.MethodGet, "/api/v1/live/account", "/api/v1/trade/account"},
		{http.MethodGet, "/api/v1/live/accounts", "/api/v1/trade/accounts"},
		{http.MethodGet, "/api/v1/live/reconciliation", "/api/v1/trade/reconciliation"},
		{http.MethodPost, "/api/v1/live/commands", "/api/v1/trade/commands"},
	}
	for _, c := range cases {
		rec := ts.do(t, c.method, c.path, nil, true)
		assert.Equal(t, http.StatusPermanentRedirect, rec.Code, c.path)
		assert.Equal(t, c.wantLocation, rec.Header().Get("Location"), c.path)
	}
}
