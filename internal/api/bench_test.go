package api

// bench_test.go is part of the permanent benchmark suite (`make bench`). It
// measures the API's heavy read endpoints end-to-end through the real chi
// router + middleware (auth, CORS, logging) + handler + JSON serialization,
// over in-memory stub stores populated with realistically large datasets. The
// DB round-trip is stubbed (the store returns its pre-built slice), so the
// number isolates the server's own per-request CPU + serialization cost — the
// component the engineer controls. Deliverable (e): API p50/p99 for the heavy
// endpoints (coverage, backtest detail).
//
// p50/p99 are computed over the per-request wall times of the timed loop and
// reported as custom metrics (in microseconds). go test's ns/op remains the
// mean; the percentiles are the additional, percentile deliverable.

import (
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/metrics"
	"github.com/byjackchen/trade-tms-go/internal/runs"
)

// benchServer wires a Server over the in-memory stubs WITHOUT a *testing.T (the
// contract-test newTestServer needs one). It mirrors that wiring exactly.
func benchServer() (*Server, *stubDataStore, *stubRunsReader) {
	cal, err := calendar.NewNYSE()
	if err != nil {
		panic(err)
	}
	jq := newStubJobQueue()
	ds := &stubDataStore{barDates: map[string][]calendar.Date{}, tickers: map[string]bool{}}
	ur := &stubUniverseReader{}
	rr := &stubRunsReader{}
	hr := &stubHyperoptReader{}
	pr := &stubPromoter{}
	srv, err := NewServer(Deps{
		Log:         zerolog.Nop(),
		Token:       testToken,
		CORSOrigins: []string{testOrigin},
		Jobs:        jq,
		Data:        ds,
		Universe:    ur,
		Runs:        rr,
		Strategies:  NewStrategyReader(nil, ""),
		Hyperopt:    hr,
		Promoter:    pr,
		Calendar:    cal,
		PingPG:      pingOK,
		PingRedis:   pingOK,
		Now:         func() time.Time { return fixedNow },
	})
	if err != nil {
		panic(err)
	}
	return srv, ds, rr
}

// benchCoverageSpans builds n per-ticker bar spans (the BarSpans payload the
// coverage gap-summary scans), with ~10 years of history each so the NYSE
// session-count expectation does real work per ticker.
func benchCoverageSpans(n int) []TickerSpan {
	spans := make([]TickerSpan, n)
	first := calendar.NewDate(2014, time.January, 2)
	last := calendar.NewDate(2024, time.January, 2)
	for i := 0; i < n; i++ {
		// Every 3rd ticker has a deliberate gap (fewer bars than expected) so the
		// worst-offender sort + summary path is exercised.
		bars := int64(2500)
		if i%3 == 0 {
			bars = int64(2000 + i%400)
		}
		spans[i] = TickerSpan{
			Ticker: tickerName(i),
			Bars:   bars,
			First:  first,
			Last:   last,
		}
	}
	return spans
}

func tickerName(i int) string {
	// Deterministic 4-letter-ish symbol.
	b := []byte{byte('A' + i%26), byte('A' + (i/26)%26), byte('A' + (i/676)%26)}
	return string(b)
}

// benchRunDetail builds a representative backtest RunDetail with portfolio +
// per-strategy metrics (the /backtests/{id} payload).
func benchRunDetail(nStrats int) *runs.RunDetail {
	fb := domain.MustMoney("1234567")
	pnl := domain.MustMoney("234567")
	sm := make(map[string]metrics.BacktestMetrics, nStrats)
	for i := 0; i < nStrats; i++ {
		sm[tickerName(i)] = metrics.BacktestMetrics{}
	}
	return &runs.RunDetail{
		RunSummary: runs.RunSummary{
			ID: 1, RunTS: "2024-01-02T00:00:00Z", Kind: "backtest", Status: "COMPLETE",
			StartDate: "2014-01-02", EndDate: "2024-01-02",
			StartingBalance: domain.MustMoney("1000000"),
			FinalBalance:    &fb, TotalPnL: &pnl,
			Strategies: func() []string {
				out := make([]string, nStrats)
				for i := range out {
					out[i] = tickerName(i)
				}
				return out
			}(),
			CreatedAt: fixedNow, UpdatedAt: fixedNow,
		},
		Config:           []byte(`{"tickers":["AAPL","MSFT"],"start":"2014-01-02","end":"2024-01-02"}`),
		PortfolioMetrics: &metrics.BacktestMetrics{},
		StrategyMetrics:  sm,
	}
}

// benchEquity builds an n-point equity curve (the /backtests/{id}/equity
// payload — typically thousands of daily points).
func benchEquity(n int) []runs.EquitySample {
	out := make([]runs.EquitySample, n)
	ts := fixedNow
	bal := int64(1_000_000_0000) // $1M in 1e-4 fixed point
	for i := 0; i < n; i++ {
		bal += int64((i*37)%1000) - 500
		out[i] = runs.EquitySample{Scope: "portfolio", TS: ts.AddDate(0, 0, i), BalanceUSD: domain.Money(bal)}
	}
	return out
}

// reportPercentiles fires the request `iters` times, records each wall time,
// and reports p50/p99/mean as custom metrics (microseconds). The first call
// must succeed (2xx); subsequent non-2xx fails the benchmark.
func reportPercentiles(b benchTB, srv *Server, target string) {
	const iters = 2000
	lat := make([]time.Duration, iters)
	for i := 0; i < iters; i++ {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.Header.Set("Authorization", "Bearer "+testToken)
		rec := httptest.NewRecorder()
		t0 := time.Now()
		srv.Routes().ServeHTTP(rec, req)
		lat[i] = time.Since(t0)
		if rec.Code != http.StatusOK {
			b.Fatalf("%s: status %d body %s", target, rec.Code, rec.Body.String())
		}
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	p := func(q float64) float64 {
		idx := int(q * float64(iters))
		if idx >= iters {
			idx = iters - 1
		}
		return float64(lat[idx].Nanoseconds()) / 1000.0 // microseconds
	}
	var sum time.Duration
	for _, d := range lat {
		sum += d
	}
	b.ReportMetric(p(0.50), "p50_us")
	b.ReportMetric(p(0.99), "p99_us")
	b.ReportMetric(float64(sum.Nanoseconds())/float64(iters)/1000.0, "mean_us")
}

// benchTB is the subset of *testing.B reportPercentiles needs.
type benchTB interface {
	Fatalf(format string, args ...any)
	ReportMetric(n float64, unit string)
}

// BenchmarkAPICoverage measures the GET /api/v1/data/coverage endpoint over a
// 5000-ticker bar-span dataset (the gap-summary scans every span against the
// NYSE session count). Reports p50/p99/mean microseconds.
func BenchmarkAPICoverage(b *testing.B) {
	srv, ds, _ := benchServer()
	ds.spans = benchCoverageSpans(5000)
	ds.coverage = []TableCoverage{{Table: "sep", Rows: 12_000_000}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/data/coverage", nil)
		req.Header.Set("Authorization", "Bearer "+testToken)
		rec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			b.Fatalf("coverage: status %d body %s", rec.Code, rec.Body.String())
		}
	}
	b.StopTimer()
	reportPercentiles(b, srv, "/api/v1/data/coverage")
}

// BenchmarkAPIBacktestDetail measures GET /api/v1/backtests/{id} over a detail
// payload with 20 per-strategy metric blocks. Reports p50/p99/mean.
func BenchmarkAPIBacktestDetail(b *testing.B) {
	srv, _, rr := benchServer()
	rr.detail = benchRunDetail(20)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/backtests/1", nil)
		req.Header.Set("Authorization", "Bearer "+testToken)
		rec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			b.Fatalf("backtest detail: status %d body %s", rec.Code, rec.Body.String())
		}
	}
	b.StopTimer()
	reportPercentiles(b, srv, "/api/v1/backtests/1")
}

// BenchmarkAPIBacktestEquity measures GET /api/v1/backtests/{id}/equity over a
// 2520-point (10y daily) equity curve — the heaviest serialization payload.
func BenchmarkAPIBacktestEquity(b *testing.B) {
	srv, _, rr := benchServer()
	rr.detail = benchRunDetail(5)
	rr.equity = benchEquity(2520)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/backtests/1/equity", nil)
		req.Header.Set("Authorization", "Bearer "+testToken)
		rec := httptest.NewRecorder()
		srv.Routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			b.Fatalf("equity: status %d body %s", rec.Code, rec.Body.String())
		}
	}
	b.StopTimer()
	reportPercentiles(b, srv, "/api/v1/backtests/1/equity")
}
