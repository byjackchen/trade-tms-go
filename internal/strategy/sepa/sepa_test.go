package sepa

// sepa_test.go covers the behavioral assertions beyond the golden dump: config
// validation, symbol gating, insufficient history, the per-rule rejection
// paths, exit-on-stop, hold-above-stop, the equity-scales-sizing invariant,
// state_dict round-trip, warmup caps, and state_summary key/value shape.

import (
	"errors"
	"testing"
	"time"
)

func drive(g *Generator, bars []Bar) []Signal {
	var out []Signal
	for _, b := range bars {
		out = append(out, g.OnBar(b)...)
	}
	return out
}

func TestConfigValidation(t *testing.T) {
	good := Config{
		Symbol: "AAPL", EquityProvider: func() float64 { return 100000 },
		RiskPct: 1.0, MarketCapMinUSD: 5e8, HardStopPct: 7.5, PivotBufferPct: 1.5,
		BreakoutVolumeMultiple: 1.5, VCPLookback: 5, HistoryMaxBars: 1000, Timezone: "America/New_York",
	}
	if _, err := New(good); err != nil {
		t.Fatalf("good config rejected: %v", err)
	}

	nilEq := good
	nilEq.EquityProvider = nil
	if _, err := New(nilEq); err == nil || !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil equity provider not rejected: %v", err)
	}

	badRisk := good
	badRisk.RiskPct = 0
	if _, err := New(badRisk); err == nil {
		t.Fatal("risk_pct=0 not rejected")
	}
	badRisk.RiskPct = 100.01
	if _, err := New(badRisk); err == nil {
		t.Fatal("risk_pct>100 not rejected")
	}

	badStop := good
	badStop.HardStopPct = 0
	if _, err := New(badStop); err == nil {
		t.Fatal("hard_stop_pct=0 not rejected")
	}
}

func TestOtherSymbolIgnored(t *testing.T) {
	g := mkSG(t, sgOpt{symbol: "AAPL", regime: "bull"})
	if got := drive(g, happyBars("MSFT")); len(got) != 0 {
		t.Fatalf("expected no signals for other symbol, got %d", len(got))
	}
	if g.Position() != 0 || len(g.close) != 0 {
		t.Fatalf("other-symbol bars must not append history (len=%d)", len(g.close))
	}
}

func TestInsufficientHistory(t *testing.T) {
	g := mkSG(t, sgOpt{regime: "bull"})
	if got := drive(g, insufficientBars()); len(got) != 0 {
		t.Fatalf("expected no signals with 60 bars, got %d", len(got))
	}
}

func TestHappyPathEntry(t *testing.T) {
	g := mkSG(t, sgOpt{regime: "bull"})
	bars := happyBars("AAPL")
	sigs := drive(g, bars)
	if len(sigs) != 1 {
		t.Fatalf("want exactly 1 signal, got %d", len(sigs))
	}
	s := sigs[0]
	if s.Side != SideLong || s.TargetQty <= 0 {
		t.Fatalf("bad entry signal: %+v", s)
	}
	if s.Grade != GradeB && s.Grade != GradeAPlus {
		t.Fatalf("unexpected grade %q", s.Grade)
	}
	if parsePyFloat(s.StopPrice) >= bars[len(bars)-1].Close {
		t.Fatalf("stop %s not below entry close %v", s.StopPrice, bars[len(bars)-1].Close)
	}
}

func TestRejectionPaths(t *testing.T) {
	cases := []struct {
		name string
		opt  sgOpt
		bars []Bar
	}{
		{"bear", sgOpt{regime: "bear"}, happyBars("AAPL")},
		{"low_cap", sgOpt{regime: "bull", marketCap: 100_000_000}, happyBars("AAPL")},
		{"blackout", sgOpt{regime: "bull", blackout: true}, happyBars("AAPL")},
		{"no_breakout", sgOpt{regime: "bull"}, noBreakoutBars()},
		{"weak_volume", sgOpt{regime: "bull"}, weakVolumeBars()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := mkSG(t, c.opt)
			if got := drive(g, c.bars); len(got) != 0 {
				t.Fatalf("%s: expected zero signals, got %d (%+v)", c.name, len(got), got)
			}
		})
	}
}

func TestUnknownAndNeutralRegimeStillEnterB(t *testing.T) {
	for _, regime := range []string{"unknown", "neutral", "warning"} {
		g := mkSG(t, sgOpt{regime: regime})
		sigs := drive(g, happyBars("AAPL"))
		if len(sigs) != 1 || sigs[0].Grade != GradeB {
			t.Fatalf("regime %q: want one B entry, got %+v", regime, sigs)
		}
	}
}

func TestExitOnStop(t *testing.T) {
	g := mkSG(t, sgOpt{regime: "bull"})
	bars := happyBars("AAPL")
	drive(g, bars)
	if g.Position() <= 0 {
		t.Fatal("expected long position after entry")
	}
	stopAt := g.StopPriceFloat()
	crash := bar("AAPL", bars[len(bars)-1].TS.AddDate(0, 0, 1), stopAt-1, stopAt, stopAt-2, stopAt-1.5, 5_000_000)
	out := g.OnBar(crash)
	if len(out) != 1 || out[0].Side != SideFlat || out[0].TargetQty != 0 {
		t.Fatalf("expected one FLAT signal, got %+v", out)
	}
	if g.Position() != 0 {
		t.Fatalf("position not cleared after stop: %d", g.Position())
	}
}

func TestHoldAboveStop(t *testing.T) {
	g := mkSG(t, sgOpt{regime: "bull"})
	bars := happyBars("AAPL")
	drive(g, bars)
	stopAt := g.StopPriceFloat()
	safe := bar("AAPL", bars[len(bars)-1].TS.AddDate(0, 0, 1), stopAt+5, stopAt+6, stopAt+4, stopAt+5, 1_500_000)
	if out := g.OnBar(safe); len(out) != 0 {
		t.Fatalf("expected no exit above stop, got %+v", out)
	}
	if g.Position() <= 0 {
		t.Fatal("position should persist above stop")
	}
}

func TestEquityScalesSizing(t *testing.T) {
	small := mkSG(t, sgOpt{regime: "bull", equity: 100000})
	large := mkSG(t, sgOpt{regime: "bull", equity: 200000})
	qs := drive(small, happyBars("AAPL"))
	ql := drive(large, happyBars("AAPL"))
	if len(qs) != 1 || len(ql) != 1 {
		t.Fatalf("expected one entry each: %d / %d", len(qs), len(ql))
	}
	expected := 2 * qs[0].TargetQty
	tol := expected / 100
	if tol < 2 {
		tol = 2
	}
	diff := ql[0].TargetQty - expected
	if diff < 0 {
		diff = -diff
	}
	if diff > tol {
		t.Fatalf("2x equity should ~2x shares: %d -> %d (expected ~%d)", qs[0].TargetQty, ql[0].TargetQty, expected)
	}
}

func TestStateDictRoundTrip(t *testing.T) {
	g := mkSG(t, sgOpt{regime: "bull"})
	drive(g, happyBars("AAPL"))
	if g.Position() <= 0 {
		t.Fatal("expected position")
	}
	snap := g.StateDict()
	// No account_size key by construction (StateConfig has none); equity snapshot present.
	if snap.Config.EquityAtSnapshot != 100000 {
		t.Fatalf("equity snapshot wrong: %v", snap.Config.EquityAtSnapshot)
	}
	g2 := mkSG(t, sgOpt{regime: "bull"})
	g2.LoadState(snap)
	if g2.Position() != g.Position() {
		t.Fatalf("position not restored: %d != %d", g2.Position(), g.Position())
	}
	if g2.stopPrice.str != g.stopPrice.str || g2.entryPrice.str != g.entryPrice.str {
		t.Fatalf("price strings not restored")
	}
	if len(g2.close) != len(g.close) {
		t.Fatalf("kline len not restored: %d != %d", len(g2.close), len(g.close))
	}
}

func TestStateDictRoundTripWhenFlat(t *testing.T) {
	g := mkSG(t, sgOpt{})
	snap := g.StateDict()
	g2 := mkSG(t, sgOpt{})
	g2.LoadState(snap)
	if g2.Position() != 0 {
		t.Fatalf("flat position not restored: %d", g2.Position())
	}
}

func TestWarmupFromHistoryCapsAtMax(t *testing.T) {
	g, err := New(Config{
		Symbol: "NVDA", EquityProvider: func() float64 { return 100000 },
		RiskPct: 1.0, MarketCapMinUSD: 5e8, HardStopPct: 7.5, PivotBufferPct: 1.5,
		BreakoutVolumeMultiple: 1.5, VCPLookback: 5, HistoryMaxBars: 200, Timezone: "America/New_York",
	})
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2024, 1, 2, 21, 0, 0, 0, time.UTC)
	var bars []Bar
	for i := 0; i < 300; i++ {
		bars = append(bars, bar("NVDA", base.AddDate(0, 0, i), 100, 101, 99, 100, 1_000_000))
	}
	g.WarmupFromHistory(bars)
	if len(g.close) != 200 {
		t.Fatalf("warmup cap: want 200, got %d", len(g.close))
	}
}

func TestWarmupEmptyIsSafe(t *testing.T) {
	g := mkSG(t, sgOpt{})
	g.WarmupFromHistory(nil)
	g.WarmupFromHistory([]Bar{})
	if len(g.close) != 0 {
		t.Fatalf("empty warmup should keep 0 bars, got %d", len(g.close))
	}
}

func TestStateSummaryKeysWhenFlat(t *testing.T) {
	g := mkSG(t, sgOpt{regime: "bull", marketCap: 10_000_000_000})
	s := g.StateSummary()
	if s.Symbol != "AAPL" || s.Regime != "bull" || s.MarketCapUSD != 1e10 {
		t.Fatalf("cold-start summary wrong: %+v", s)
	}
	if s.InBlackout || s.PositionQty != 0 || s.EntryPrice != "" || s.StopPrice != "" ||
		s.CurrentGrade != "" || s.VCPDetected || s.PivotPrice != "" {
		t.Fatalf("flat summary should be empty: %+v", s)
	}
}
