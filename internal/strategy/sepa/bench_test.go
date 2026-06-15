package sepa

// bench_test.go measures the per-OnBar cost of the flat-book entry chain
// (ClassifyStage + EvaluateTrendTemplate + DetectVCP) over a full ~1000-bar
// history — the hot path that fix 1/2/3 target. The generator is kept flat
// (low market cap so it never enters) so every bar exercises the full
// indicator chain.

import (
	"testing"
	"time"
)

// benchBars builds a long uphill-then-noise close series of n bars that keeps
// the book flat (never breaks out) so every OnBar runs the entry chain.
func benchBars(n int) []Bar {
	bars := make([]Bar, n)
	ts := time.Date(2020, 1, 2, 21, 0, 0, 0, time.UTC)
	c := 50.0
	for i := 0; i < n; i++ {
		// gentle deterministic oscillating uptrend
		c += 0.05
		if i%7 == 0 {
			c -= 0.18
		}
		if c < 10 {
			c = 10
		}
		bars[i] = bar("AAPL", ts.AddDate(0, 0, i), c, c+0.5, c-0.5, c, 1_000_000)
	}
	return bars
}

func benchGen(b *testing.B) *Generator {
	g, err := New(Config{
		Symbol:                 "AAPL",
		EquityProvider:         func() float64 { return 100000 },
		RiskPct:                1.0,
		MarketCapMinUSD:        500_000_000.0,
		HardStopPct:            7.5,
		PivotBufferPct:         1.5,
		BreakoutVolumeMultiple: 1.5,
		VCPLookback:            4,
		HistoryMaxBars:         1000,
		Timezone:               "America/New_York",
	})
	if err != nil {
		b.Fatal(err)
	}
	g.SetRegime("bull")
	g.SetMarketCap(10_000_000_000.0) // passes cap gate; stays flat via no breakout
	return g
}

// BenchmarkOnBarSteadyState warms the buffer to its cap (1000 bars) then times
// OnBar on a buffer that is always at cap — the worst case the profile flagged.
func BenchmarkOnBarSteadyState(b *testing.B) {
	warm := benchBars(1200)
	g := benchGen(b)
	for _, bar := range warm {
		g.OnBar(bar)
	}
	// One extra bar template to feed repeatedly (kept flat).
	ts := warm[len(warm)-1].TS
	c := warm[len(warm)-1].Close
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ts = ts.AddDate(0, 0, 1)
		nb := bar("AAPL", ts, c, c+0.5, c-0.5, c, 1_000_000)
		g.OnBar(nb)
	}
}

// BenchmarkOnBarLowCapEarlyReject exercises fix 3: a sub-min-cap name is
// rejected by the hoisted market-cap gate BEFORE any indicator work, so OnBar is
// nearly free (just appendBar's incremental maintenance + the cap compare).
func BenchmarkOnBarLowCapEarlyReject(b *testing.B) {
	warm := benchBars(1200)
	g := benchGen(b)
	g.SetMarketCap(100_000_000.0) // below the 500M min -> rule 8 fails
	for _, bar := range warm {
		g.OnBar(bar)
	}
	ts := warm[len(warm)-1].TS
	c := warm[len(warm)-1].Close
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ts = ts.AddDate(0, 0, 1)
		nb := bar("AAPL", ts, c, c+0.5, c-0.5, c, 1_000_000)
		g.OnBar(nb)
	}
}
