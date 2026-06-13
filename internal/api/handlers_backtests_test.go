package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/jobs/handlers"
	"github.com/byjackchen/trade-tms-go/internal/metrics"
	"github.com/byjackchen/trade-tms-go/internal/runs"
)

// ---------------------------------------------------------------------------
// POST /api/v1/backtests
// ---------------------------------------------------------------------------

func TestBacktestEnqueue(t *testing.T) {
	t.Run("auth required", func(t *testing.T) {
		ts := newTestServer(t)
		rec := ts.do(t, http.MethodPost, "/api/v1/backtests", strings.NewReader(`{}`), false)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})
	t.Run("missing start/end is 400", func(t *testing.T) {
		ts := newTestServer(t)
		rec := ts.do(t, http.MethodPost, "/api/v1/backtests",
			strings.NewReader(`{"tickers":["AAPL"]}`), true)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Equal(t, CodeValidation, errCode(t, rec))
	})
	t.Run("no tickers and no universe is 400", func(t *testing.T) {
		ts := newTestServer(t)
		rec := ts.do(t, http.MethodPost, "/api/v1/backtests",
			strings.NewReader(`{"start":"2024-01-02","end":"2024-12-31"}`), true)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})
	t.Run("unknown fill_profile is 400", func(t *testing.T) {
		ts := newTestServer(t)
		rec := ts.do(t, http.MethodPost, "/api/v1/backtests",
			strings.NewReader(`{"start":"2024-01-02","end":"2024-12-31","tickers":["AAPL"],"fill_profile":"magic"}`), true)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})
	t.Run("unsupported strategy is 400", func(t *testing.T) {
		ts := newTestServer(t)
		rec := ts.do(t, http.MethodPost, "/api/v1/backtests",
			strings.NewReader(`{"start":"2024-01-02","end":"2024-12-31","tickers":["AAPL"],"strategy":"sepa"}`), true)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})
	t.Run("unknown json field is 400", func(t *testing.T) {
		ts := newTestServer(t)
		rec := ts.do(t, http.MethodPost, "/api/v1/backtests",
			strings.NewReader(`{"start":"2024-01-02","end":"2024-12-31","tickers":["AAPL"],"bogus":1}`), true)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})
	t.Run("max_attempts out of range is 400", func(t *testing.T) {
		ts := newTestServer(t)
		rec := ts.do(t, http.MethodPost, "/api/v1/backtests",
			strings.NewReader(`{"start":"2024-01-02","end":"2024-12-31","tickers":["AAPL"],"max_attempts":99}`), true)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})
	t.Run("happy path enqueues 202 and audits actor", func(t *testing.T) {
		ts := newTestServer(t)
		body := `{"start":"2024-01-02","end":"2024-12-31","tickers":["AAPL","KO"],` +
			`"fill_profile":"nautilus-compat","kind":"smoke","actor":"bob",` +
			`"intents":[{"date":"2024-01-03","ticker":"AAPL","side":"LONG","qty":100}]}`
		rec := ts.do(t, http.MethodPost, "/api/v1/backtests", strings.NewReader(body), true)
		require.Equal(t, http.StatusAccepted, rec.Code)
		out := decodeBody(t, rec)
		job := out["job"].(map[string]any)
		assert.NotZero(t, job["id"])

		require.Len(t, ts.jobs.enqueued, 1)
		p := ts.jobs.enqueued[0]
		assert.Equal(t, handlers.KindBacktestRun, p.Kind)
		assert.Equal(t, "api:bob", p.Actor)
		payload := p.Payload.(map[string]any)
		assert.Equal(t, "2024-01-02", payload["start"])
		assert.Equal(t, []string{"AAPL", "KO"}, payload["tickers"])
		assert.Equal(t, "smoke", payload["kind"])
		assert.NotNil(t, payload["intents"])
	})
}

// ---------------------------------------------------------------------------
// GET /api/v1/backtests
// ---------------------------------------------------------------------------

func TestBacktestList(t *testing.T) {
	t.Run("empty list returns []", func(t *testing.T) {
		ts := newTestServer(t)
		rec := ts.do(t, http.MethodGet, "/api/v1/backtests", nil, true)
		require.Equal(t, http.StatusOK, rec.Code)
		out := decodeBody(t, rec)
		assert.Equal(t, []any{}, out["backtests"])
	})
	t.Run("returns summaries newest-first as stored", func(t *testing.T) {
		ts := newTestServer(t)
		final := domain.MustMoney("105000.00")
		pnl := domain.MustMoney("5000.00")
		ts.runs.list = []runs.RunSummary{
			{
				ID: 2, RunTS: "2024-06-13_12-00-00", Kind: "multi-strategy", Status: "COMPLETE",
				StartDate: "2024-01-02", EndDate: "2024-12-31",
				StartingBalance: domain.MustMoney("100000.00"), FinalBalance: &final, TotalPnL: &pnl,
				Strategies: []string{"Scripted-000"}, CreatedAt: fixedNow, UpdatedAt: fixedNow,
			},
		}
		rec := ts.do(t, http.MethodGet, "/api/v1/backtests?status=COMPLETE&kind=multi-strategy&limit=10", nil, true)
		require.Equal(t, http.StatusOK, rec.Code)
		out := decodeBody(t, rec)
		list := out["backtests"].([]any)
		require.Len(t, list, 1)
		row := list[0].(map[string]any)
		assert.Equal(t, float64(2), row["id"])
		assert.Equal(t, "COMPLETE", row["status"])
		assert.Equal(t, 105000.0, row["final_balance_usd"])
		assert.Equal(t, 5000.0, row["total_pnl_usd"])
		// Filter propagated.
		assert.Equal(t, "COMPLETE", ts.runs.lastListFilter.Status)
		assert.Equal(t, "multi-strategy", ts.runs.lastListFilter.Kind)
		assert.Equal(t, 10, ts.runs.lastListFilter.Limit)
	})
	t.Run("invalid status is 400", func(t *testing.T) {
		ts := newTestServer(t)
		rec := ts.do(t, http.MethodGet, "/api/v1/backtests?status=BOGUS", nil, true)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})
	t.Run("invalid limit is 400", func(t *testing.T) {
		ts := newTestServer(t)
		rec := ts.do(t, http.MethodGet, "/api/v1/backtests?limit=0", nil, true)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})
	t.Run("db error is 500", func(t *testing.T) {
		ts := newTestServer(t)
		ts.runs.err = assertErr("boom")
		rec := ts.do(t, http.MethodGet, "/api/v1/backtests", nil, true)
		assert.Equal(t, http.StatusInternalServerError, rec.Code)
	})
}

// ---------------------------------------------------------------------------
// GET /api/v1/backtests/{id}
// ---------------------------------------------------------------------------

func TestBacktestGet(t *testing.T) {
	t.Run("bad id is 400", func(t *testing.T) {
		ts := newTestServer(t)
		rec := ts.do(t, http.MethodGet, "/api/v1/backtests/abc", nil, true)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})
	t.Run("not found is 404", func(t *testing.T) {
		ts := newTestServer(t)
		ts.runs.notFound = true
		rec := ts.do(t, http.MethodGet, "/api/v1/backtests/7", nil, true)
		assert.Equal(t, http.StatusNotFound, rec.Code)
		assert.Equal(t, CodeNotFound, errCode(t, rec))
	})
	t.Run("detail returns meta + metrics", func(t *testing.T) {
		ts := newTestServer(t)
		final := domain.MustMoney("105000.00")
		pnl := domain.MustMoney("5000.00")
		pm := metrics.BacktestMetrics{
			FinalBalanceUSD: 105000, TotalPnLUSD: 5000, Sharpe: 1.5, Calmar: 2.5,
			MaxDrawdownPct: -3.2, NumOrders: 4, NumFilledOrders: 4, NumPositions: 2,
		}
		ts.runs.detail = &runs.RunDetail{
			RunSummary: runs.RunSummary{
				ID: 7, RunTS: "2024-06-13_12-00-00", Kind: "multi-strategy", Status: "COMPLETE",
				StartDate: "2024-01-02", EndDate: "2024-12-31",
				StartingBalance: domain.MustMoney("100000.00"), FinalBalance: &final, TotalPnL: &pnl,
				Strategies: []string{"Scripted-000"}, CreatedAt: fixedNow, UpdatedAt: fixedNow,
			},
			Config:           json.RawMessage(`{"start":"2024-01-02"}`),
			PortfolioMetrics: &pm,
			StrategyMetrics:  map[string]metrics.BacktestMetrics{"Scripted-000": pm},
		}
		rec := ts.do(t, http.MethodGet, "/api/v1/backtests/7", nil, true)
		require.Equal(t, http.StatusOK, rec.Code)
		out := decodeBody(t, rec)
		bt := out["backtest"].(map[string]any)
		assert.Equal(t, float64(7), bt["id"])
		m := out["metrics"].(map[string]any)
		assert.Equal(t, 1.5, m["sharpe"])
		assert.Equal(t, 2.5, m["calmar"])
		assert.Equal(t, -3.2, m["max_drawdown_pct"])
		assert.Equal(t, float64(4), m["num_orders"])
		sm := out["strategy_metrics"].(map[string]any)
		assert.Contains(t, sm, "Scripted-000")
		cfg := out["config"].(map[string]any)
		assert.Equal(t, "2024-01-02", cfg["start"])
	})
}

// ---------------------------------------------------------------------------
// GET /api/v1/backtests/{id}/equity
// ---------------------------------------------------------------------------

func TestBacktestEquity(t *testing.T) {
	t.Run("not found is 404", func(t *testing.T) {
		ts := newTestServer(t)
		ts.runs.notFound = true
		rec := ts.do(t, http.MethodGet, "/api/v1/backtests/7/equity", nil, true)
		assert.Equal(t, http.StatusNotFound, rec.Code)
	})
	t.Run("portfolio scope by default", func(t *testing.T) {
		ts := newTestServer(t)
		ts.runs.equity = []runs.EquitySample{
			{Scope: "portfolio", TS: fixedNow, BalanceUSD: domain.MustMoney("100000.00")},
			{Scope: "portfolio", TS: fixedNow.Add(24 * time.Hour), BalanceUSD: domain.MustMoney("100500.00")},
		}
		rec := ts.do(t, http.MethodGet, "/api/v1/backtests/7/equity", nil, true)
		require.Equal(t, http.StatusOK, rec.Code)
		out := decodeBody(t, rec)
		assert.Equal(t, "portfolio", out["scope"])
		pts := out["points"].([]any)
		require.Len(t, pts, 2)
		assert.Equal(t, 100000.0, pts[0].(map[string]any)["balance_usd"])
		// The handler forwards the empty scope; the store resolves "" -> portfolio.
		assert.Equal(t, "", ts.runs.lastEquityScope)
	})
	t.Run("strategy scope passed through", func(t *testing.T) {
		ts := newTestServer(t)
		rec := ts.do(t, http.MethodGet, "/api/v1/backtests/7/equity?strategy=Scripted-000", nil, true)
		require.Equal(t, http.StatusOK, rec.Code)
		out := decodeBody(t, rec)
		assert.Equal(t, "Scripted-000", out["scope"])
		assert.Equal(t, "Scripted-000", ts.runs.lastEquityScope)
	})
}

// ---------------------------------------------------------------------------
// GET /api/v1/backtests/{id}/trades
// ---------------------------------------------------------------------------

func TestBacktestTrades(t *testing.T) {
	t.Run("not found is 404", func(t *testing.T) {
		ts := newTestServer(t)
		ts.runs.notFound = true
		rec := ts.do(t, http.MethodGet, "/api/v1/backtests/7/trades", nil, true)
		assert.Equal(t, http.StatusNotFound, rec.Code)
	})
	t.Run("returns trades", func(t *testing.T) {
		ts := newTestServer(t)
		exitTS := fixedNow.Add(48 * time.Hour)
		exitPx := domain.MustPrice("12.00")
		pnl := domain.MustMoney("200.00")
		ts.runs.trades = []runs.TradeRow{
			{
				ID: 1, StrategyID: "Scripted-000", Symbol: "AAPL", Side: "LONG", Qty: 100,
				EntryTS: fixedNow, ExitTS: &exitTS, EntryPx: domain.MustPrice("10.00"),
				ExitPx: &exitPx, RealizedPnL: &pnl,
			},
		}
		rec := ts.do(t, http.MethodGet, "/api/v1/backtests/7/trades", nil, true)
		require.Equal(t, http.StatusOK, rec.Code)
		out := decodeBody(t, rec)
		trades := out["trades"].([]any)
		require.Len(t, trades, 1)
		tr := trades[0].(map[string]any)
		assert.Equal(t, "AAPL", tr["symbol"])
		assert.Equal(t, "LONG", tr["side"])
		assert.Equal(t, float64(100), tr["qty"])
		assert.Equal(t, 10.0, tr["entry_px"])
		assert.Equal(t, 12.0, tr["exit_px"])
		assert.Equal(t, 200.0, tr["realized_pnl_usd"])
	})
	t.Run("open trade has null exit", func(t *testing.T) {
		ts := newTestServer(t)
		ts.runs.trades = []runs.TradeRow{
			{ID: 1, StrategyID: "S", Symbol: "X", Side: "LONG", Qty: 10, EntryTS: fixedNow, EntryPx: domain.MustPrice("5.00")},
		}
		rec := ts.do(t, http.MethodGet, "/api/v1/backtests/7/trades", nil, true)
		require.Equal(t, http.StatusOK, rec.Code)
		out := decodeBody(t, rec)
		tr := out["trades"].([]any)[0].(map[string]any)
		assert.Nil(t, tr["exit_ts"])
		assert.Nil(t, tr["exit_px"])
	})
}

// ---------------------------------------------------------------------------
// GET /api/v1/backtests/{id}/orders
// ---------------------------------------------------------------------------

func TestBacktestOrders(t *testing.T) {
	t.Run("not found is 404", func(t *testing.T) {
		ts := newTestServer(t)
		ts.runs.notFound = true
		rec := ts.do(t, http.MethodGet, "/api/v1/backtests/7/orders", nil, true)
		assert.Equal(t, http.StatusNotFound, rec.Code)
	})
	t.Run("passes through the stored orders array", func(t *testing.T) {
		ts := newTestServer(t)
		ts.runs.orders = json.RawMessage(`[{"client_order_id":"c1","side":"BUY"}]`)
		rec := ts.do(t, http.MethodGet, "/api/v1/backtests/7/orders", nil, true)
		require.Equal(t, http.StatusOK, rec.Code)
		var arr []map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &arr))
		require.Len(t, arr, 1)
		assert.Equal(t, "c1", arr[0]["client_order_id"])
	})
	t.Run("empty stored orders yields []", func(t *testing.T) {
		ts := newTestServer(t)
		ts.runs.orders = nil
		rec := ts.do(t, http.MethodGet, "/api/v1/backtests/7/orders", nil, true)
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "[]", strings.TrimSpace(rec.Body.String()))
	})
}
