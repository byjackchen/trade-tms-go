package sepa

// incgolden_test.go is the BYTE-FOR-BYTE proof that the incremental entry-chain
// classifiers (classifyStageInc / trendTemplatePassInc / maSlopePct200x20 /
// fractionAbove200 / the 52wk high-low) reproduce the batch internal/indicators
// routines they replaced (ClassifyStage / EvaluateTrendTemplate /
// MASlopePct / FractionAbove / FiftyTwoWeekHigh-Low) EXACTLY at every bar —
// including across the HistoryMaxBars front-trim (the ring/reslice path), which
// the ~261-bar golden fixtures never reach. A single-ULP MA drift would flip a
// strict trend-template comparison; this asserts zero drift on long streams.

import (
	"math"
	"math/rand"
	"testing"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/indicators"
)

func bitEq(a, b float64) bool {
	if math.IsNaN(a) && math.IsNaN(b) {
		return true
	}
	return a == b // exact float compare (bit-for-bit)
}

// TestIncrementalEntryChainGolden drives several long synthetic series through a
// generator and, at every bar with enough history, compares the incremental
// classifiers against the batch indicators over the SAME live buffer g.close /
// g.high / g.low. Any mismatch (stage string, tt-pass bool, or the underlying
// MA / 52wk / slope / fraction floats) fails.
func TestIncrementalEntryChainGolden(t *testing.T) {
	seeds := []int64{1, 7, 42, 2024}
	caps := []int{300, 1000} // HistoryMaxBars: short (no trim) and the prod cap

	for _, histMax := range caps {
		for _, seed := range seeds {
			g, err := New(Config{
				Symbol:                 "AAPL",
				EquityProvider:         func() float64 { return 100000 },
				RiskPct:                1.0,
				MarketCapMinUSD:        500_000_000.0,
				HardStopPct:            7.5,
				PivotBufferPct:         1.5,
				BreakoutVolumeMultiple: 1.5,
				VCPLookback:            4,
				HistoryMaxBars:         histMax,
				Timezone:               "America/New_York",
			})
			if err != nil {
				t.Fatal(err)
			}
			g.SetRegime("bull")
			g.SetMarketCap(10_000_000_000.0)

			rng := rand.New(rand.NewSource(seed))
			ts := time.Date(2018, 1, 2, 21, 0, 0, 0, time.UTC)
			c := 50.0
			// Long enough to exercise the trim at least twice for histMax=1000.
			nBars := histMax + 1500
			for i := 0; i < nBars; i++ {
				c += rng.NormFloat64() * 0.4
				if c < 5 {
					c = 5
				}
				hi := c + 0.5
				lo := c - 0.5
				b := bar("AAPL", ts.AddDate(0, 0, i), c, hi, lo, c, 1_000_000)
				// Drive through OnBar so the buffer + inc state advance together
				// (book stays flat: stage/breakout reject random noise).
				g.OnBar(b)

				n := len(g.close)
				if n < indicators.StageMinBars {
					continue
				}

				// --- stage equivalence ---
				wantStage := indicators.ClassifyStage(g.close)
				gotStage := g.classifyStageInc()
				if wantStage != gotStage {
					t.Fatalf("hist=%d seed=%d bar=%d: stage %q != batch %q",
						histMax, seed, i, gotStage, wantStage)
				}

				// --- trend-template Passed() equivalence (n>=200 here) ---
				if n >= indicators.TTMinBars {
					wantTT := indicators.EvaluateTrendTemplate(
						g.close, g.high, g.low, g.marketCapUSD, g.cfg.MarketCapMinUSD,
					).Passed()
					gotTT := g.trendTemplatePassInc()
					if wantTT != gotTT {
						t.Fatalf("hist=%d seed=%d bar=%d: tt-pass %v != batch %v",
							histMax, seed, i, gotTT, wantTT)
					}
				}

				// --- underlying float equivalence (bit-for-bit) ---
				wantSlope := indicators.MASlopePct(g.close, 200, 20)
				if gotSlope := g.maSlopePct200x20(); !bitEq(gotSlope, wantSlope) {
					t.Fatalf("hist=%d seed=%d bar=%d: slope %.17g != batch %.17g",
						histMax, seed, i, gotSlope, wantSlope)
				}
				wantFrac := indicators.FractionAbove(g.close, indicators.MA(g.close, 200), 200)
				if gotFrac := g.fractionAbove200(); !bitEq(gotFrac, wantFrac) {
					t.Fatalf("hist=%d seed=%d bar=%d: fracAbove %.17g != batch %.17g",
						histMax, seed, i, gotFrac, wantFrac)
				}
				wantHi := indicators.FiftyTwoWeekHigh(g.high, indicators.TTHighLowWindow)
				if gotHi := g.inc.fiftyTwoWeekHigh(); !bitEq(gotHi, wantHi) {
					t.Fatalf("hist=%d seed=%d bar=%d: 52wkHigh %.17g != batch %.17g",
						histMax, seed, i, gotHi, wantHi)
				}
				wantLo := indicators.FiftyTwoWeekLow(g.low, indicators.TTHighLowWindow)
				if gotLo := g.inc.fiftyTwoWeekLow(); !bitEq(gotLo, wantLo) {
					t.Fatalf("hist=%d seed=%d bar=%d: 52wkLow %.17g != batch %.17g",
						histMax, seed, i, gotLo, wantLo)
				}
				// MA50/150/200 last-value equivalence.
				if got, want := freshSMA(g.close, 50), lastVal(indicators.MA(g.close, 50)); !bitEq(got, want) {
					t.Fatalf("hist=%d seed=%d bar=%d: ma50 %.17g != batch %.17g", histMax, seed, i, got, want)
				}
				if got, want := freshSMA(g.close, 150), lastVal(indicators.MA(g.close, 150)); !bitEq(got, want) {
					t.Fatalf("hist=%d seed=%d bar=%d: ma150 %.17g != batch %.17g", histMax, seed, i, got, want)
				}
				if got, want := g.inc.ma200[len(g.inc.ma200)-1], lastVal(indicators.MA(g.close, 200)); !bitEq(got, want) {
					t.Fatalf("hist=%d seed=%d bar=%d: ma200 %.17g != batch %.17g", histMax, seed, i, got, want)
				}
			}
		}
	}
}

func lastVal(s []float64) float64 {
	if len(s) == 0 {
		return math.NaN()
	}
	return s[len(s)-1]
}
