package sepa

// fixtures_test.go reproduces the Python reference fixtures
// (tests/strategies/sepa/test_signal.py + tmp/parity_sepa/dump_py.py)
// bit-for-bit in Go: numpy.linspace replicated exactly (so the synthetic OHLC
// float64 values are IEEE-identical to the reference), and the same bar
// construction (high=close+0.5, low=close-0.5, the dryup volume window, the
// breakout bar). The bar.close the reference feeds is float(Decimal(str(np))),
// which equals the np float64 exactly; we use the linspace float64 directly.

import (
	"testing"
	"time"
)

var baseTS = time.Date(2023, 1, 2, 21, 0, 0, 0, time.UTC)

// npLinspace reproduces numpy.linspace(start, stop, num) EXACTLY (the endpoint
// variant numpy uses by default): step = (stop-start)/(num-1); y[i] = i*step +
// start; y[num-1] = stop (the explicit endpoint assignment numpy performs).
func npLinspace(start, stop float64, num int) []float64 {
	out := make([]float64, num)
	if num == 1 {
		out[0] = start
		return out
	}
	div := float64(num - 1)
	step := (stop - start) / div
	for i := 0; i < num; i++ {
		out[i] = float64(i)*step + start
	}
	out[num-1] = stop
	return out
}

func bar(symbol string, ts time.Time, o, h, lo, c float64, v int64) Bar {
	return Bar{Symbol: symbol, TS: ts, Open: o, High: h, Low: lo, Close: c, Volume: v}
}

// happyCloses builds the 260-bar close series (200 uptrend + 60 VCP base).
func happyCloses() []float64 {
	uptrend := npLinspace(50.0, 115.0, 200)
	riseA := npLinspace(115.0, 120.0, 12)
	dropA := npLinspace(120.0, 109.0, 12)
	riseB := npLinspace(109.0, 118.0, 12)
	dropB := npLinspace(118.0, 113.0, 12)
	coil := npLinspace(113.0, 117.5, 12)
	closes := make([]float64, 0, 260)
	closes = append(closes, uptrend...)
	closes = append(closes, riseA...)
	closes = append(closes, dropA...)
	closes = append(closes, riseB...)
	closes = append(closes, dropB...)
	closes = append(closes, coil...)
	return closes
}

// happyBars builds the 261-bar happy-path series (260 base + 1 breakout).
func happyBars(symbol string) []Bar {
	closes := happyCloses()
	volumes := make([]int64, len(closes))
	for i := range volumes {
		volumes[i] = 1_000_000
	}
	// Drop volume during the final contraction (drop_b): closes[212+36:212+48].
	for i := 212 + 36; i < 212+48; i++ {
		volumes[i] = 600_000
	}
	bars := make([]Bar, 0, len(closes)+1)
	for i, c := range closes {
		ts := baseTS.AddDate(0, 0, i)
		bars = append(bars, bar(symbol, ts, c, c+0.5, c-0.5, c, volumes[i]))
	}
	breakoutTS := baseTS.AddDate(0, 0, len(closes))
	bars = append(bars, bar(symbol, breakoutTS, 117.5, 122.0, 117.0, 121.0, 3_000_000))
	return bars
}

func noBreakoutBars() []Bar {
	bars := happyBars("AAPL")
	last := bars[len(bars)-1]
	bars[len(bars)-1] = bar(last.Symbol, last.TS, 117.0, 117.2, 116.8, 117.0, 3_000_000)
	return bars
}

func weakVolumeBars() []Bar {
	bars := happyBars("AAPL")
	last := bars[len(bars)-1]
	bars[len(bars)-1] = bar(last.Symbol, last.TS, 117.5, 122.0, 117.0, 121.0, 800_000)
	return bars
}

func insufficientBars() []Bar {
	bars := make([]Bar, 0, 60)
	for i := 0; i < 60; i++ {
		ts := baseTS.AddDate(0, 0, i)
		bars = append(bars, bar("AAPL", ts, 100, 100.5, 99.5, 100, 1_000_000))
	}
	return bars
}

// exitOnStopBars drives the happy path to entry, reads the resulting stop, then
// appends a crash bar below it (mirroring dump_py.py scenario_exit_on_stop's
// re-run over the full series).
func exitOnStopBars(t *testing.T) []Bar {
	t.Helper()
	g := mkSG(t, sgOpt{regime: "bull"})
	bars := happyBars("AAPL")
	for _, b := range bars {
		g.OnBar(b)
	}
	stopAt := g.StopPriceFloat()
	crashTS := bars[len(bars)-1].TS.AddDate(0, 0, 1)
	crash := bar("AAPL", crashTS, stopAt-1, stopAt, stopAt-2, stopAt-1.5, 5_000_000)
	return append(bars, crash)
}

// holdAboveStopBars is the symmetric "stays above stop" series.
func holdAboveStopBars(t *testing.T) []Bar {
	t.Helper()
	g := mkSG(t, sgOpt{regime: "bull"})
	bars := happyBars("AAPL")
	for _, b := range bars {
		g.OnBar(b)
	}
	stopAt := g.StopPriceFloat()
	safeTS := bars[len(bars)-1].TS.AddDate(0, 0, 1)
	safe := bar("AAPL", safeTS, stopAt+5, stopAt+6, stopAt+4, stopAt+5, 1_500_000)
	return append(bars, safe)
}

// ---- generator builder ----------------------------------------------------

type sgOpt struct {
	symbol    string
	marketCap float64
	regime    string
	blackout  bool
	catalyst  bool
	equity    float64
}

func mkSG(t *testing.T, o sgOpt) *Generator {
	t.Helper()
	if o.symbol == "" {
		o.symbol = "AAPL"
	}
	if o.marketCap == 0 {
		o.marketCap = 10_000_000_000.0
	}
	if o.equity == 0 {
		o.equity = 100000
	}
	equity := o.equity
	g, err := NewGenerator(Config{
		Symbol:                 o.symbol,
		EquityProvider:         func() float64 { return equity },
		RiskPct:                1.0,
		MarketCapMinUSD:        500_000_000.0,
		HardStopPct:            7.5,
		PivotBufferPct:         1.5,
		BreakoutVolumeMultiple: 1.5,
		VCPLookback:            4, // matches the reference VCP fixture geometry
		HistoryMaxBars:         1000,
		Timezone:               "America/New_York",
	})
	if err != nil {
		t.Fatalf("NewGenerator: %v", err)
	}
	g.SetMarketCap(o.marketCap)
	g.SetRegime(o.regime)
	g.SetEarningsBlackout(o.blackout)
	g.SetCatalyst(o.catalyst)
	return g
}
