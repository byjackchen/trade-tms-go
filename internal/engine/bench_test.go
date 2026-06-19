package engine

// bench_test.go is part of the permanent benchmark suite (`make bench`). It
// measures the DETERMINISTIC backtest engine throughput in bars/sec over a
// multi-year, multi-strategy, multi-instrument run, using a synthetic but
// realistically-shaped feed and scripted strategies (so the benchmark has no
// external data dependency and is fully reproducible).
//
// The benchmark is hermetic: it builds its own in-memory SliceFeed and runs the
// SAME engine.New/Run path as a real backtest. The custom `bars/sec` metric is
// the deliverable for docs/reference/benchmarks.md (a).

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// benchWeekdays returns n consecutive weekday (Mon-Fri) UTC-midnight dates
// starting at start, approximating a US trading calendar's density (~252/yr)
// without holidays (holiday gaps do not change per-bar engine cost).
func benchWeekdays(start time.Time, n int) []time.Time {
	out := make([]time.Time, 0, n)
	d := start
	for len(out) < n {
		if wd := d.Weekday(); wd != time.Saturday && wd != time.Sunday {
			out = append(out, d)
		}
		d = d.AddDate(0, 0, 1)
	}
	return out
}

// benchSyntheticBars builds a deterministic OHLCV series for one symbol over the
// given dates. Prices follow a bounded oscillation so the fixed-point math and
// mark-to-market exercise realistic magnitudes; values are reproducible.
func benchSyntheticBars(symbol string, dates []time.Time, seed int) InstrumentBars {
	bars := make([]domain.Bar, 0, len(dates))
	base := int64(5000 + seed*137) // ~$50 + per-symbol offset (dollars)
	for i, ts := range dates {
		osc := int64((i*31+seed*17)%2000) - 1000 // [-1000,1000]
		c := (base + osc) * 100                  // to 1e-4 fixed point
		o := c - 50
		h := c + 200
		l := c - 200
		bars = append(bars, domain.Bar{
			Symbol: symbol,
			TS:     ts,
			Open:   domain.Price(o),
			High:   domain.Price(h),
			Low:    domain.Price(l),
			Close:  domain.Price(c),
			Volume: 1_000_000 + int64(i),
		})
	}
	return InstrumentBars{Symbol: symbol, Bars: bars}
}

// benchScriptedIntents builds a steady stream of alternating long/flat intents
// for a symbol on every k-th bar, so the executor, accounting and fill path are
// continuously exercised (not a no-trade run).
func benchScriptedIntents(symbol string, dates []time.Time, everyK int) []Intent {
	intents := make([]Intent, 0, len(dates)/everyK+1)
	long := true
	for i := 0; i < len(dates); i += everyK {
		side := domain.SideLong
		if !long {
			side = domain.SideFlat
		}
		intents = append(intents, Intent{Date: dates[i], Ticker: symbol, Side: side, Qty: 100})
		long = !long
	}
	return intents
}

// benchEngineConfig assembles a multi-year, multi-strategy, multi-instrument
// backtest: nSymbols instruments over nYears (~252 bars/yr), each driven by its
// own scripted strategy trading every 5th bar. Returns the config, the feed and
// the total bar count.
func benchEngineConfig(nSymbols, nYears int) (Config, SliceFeed, int) {
	dates := benchWeekdays(time.Date(2010, 1, 4, 0, 0, 0, 0, time.UTC), nYears*252)
	tickers := make([]string, 0, nSymbols)
	instruments := make([]InstrumentBars, 0, nSymbols)
	specs := make([]StrategySpec, 0, nSymbols)
	totalBars := 0
	for s := 0; s < nSymbols; s++ {
		sym := fmt.Sprintf("SYM%03d", s)
		tickers = append(tickers, sym)
		ib := benchSyntheticBars(sym, dates, s)
		instruments = append(instruments, ib)
		totalBars += len(ib.Bars)
		specs = append(specs, StrategySpec{
			ID:      fmt.Sprintf("Scripted-%03d", s),
			Intents: benchScriptedIntents(sym, dates, 5),
		})
	}
	cfg := Config{
		Tickers:         tickers,
		Start:           calendar.NewDate(2010, time.January, 1),
		End:             calendar.NewDate(2010+nYears+1, time.December, 31),
		StartingBalance: domain.MustMoney("1000000"),
		Profile:         ProfileCloseFill,
		Strategies:      specs,
	}
	return cfg, SliceFeed{Instruments: instruments}, totalBars
}

// runBenchEngine builds + runs one engine over the config/feed, returning the
// bars processed (for the bars/sec metric). Fails the benchmark on any error.
func runBenchEngine(b *testing.B, cfg Config, feed SliceFeed) int {
	b.Helper()
	eng, err := New(context.Background(), cfg, feed)
	if err != nil {
		b.Fatalf("engine.New: %v", err)
	}
	res, err := eng.Run(context.Background())
	if err != nil {
		b.Fatalf("engine.Run: %v", err)
	}
	return res.BarsProcessed
}

func reportBarsPerSec(b *testing.B, bars int) {
	b.Helper()
	secPerOp := float64(b.Elapsed().Nanoseconds()) / float64(b.N) / 1e9
	if secPerOp > 0 {
		b.ReportMetric(float64(bars)/secPerOp, "bars/sec")
	}
}

// BenchmarkEngineThroughput_5y_5sym is the headline backtest-engine throughput
// number: 5 years x 5 instruments x 5 scripted strategies (~6300 bars). The
// custom `bars/sec` metric is the deliverable for docs/reference/benchmarks.md (a).
func BenchmarkEngineThroughput_5y_5sym(b *testing.B) {
	cfg, feed, _ := benchEngineConfig(5, 5)
	b.ReportAllocs()
	b.ResetTimer()
	var bars int
	for i := 0; i < b.N; i++ {
		bars = runBenchEngine(b, cfg, feed)
	}
	b.StopTimer()
	reportBarsPerSec(b, bars)
}

// BenchmarkEngineThroughput_10y_20sym is the heavier, more representative run:
// 10 years x 20 instruments (~50k bars), the kind of universe a real
// multi-strategy backtest sweeps. Same bars/sec deliverable.
func BenchmarkEngineThroughput_10y_20sym(b *testing.B) {
	cfg, feed, _ := benchEngineConfig(20, 10)
	b.ReportAllocs()
	b.ResetTimer()
	var bars int
	for i := 0; i < b.N; i++ {
		bars = runBenchEngine(b, cfg, feed)
	}
	b.StopTimer()
	reportBarsPerSec(b, bars)
}
