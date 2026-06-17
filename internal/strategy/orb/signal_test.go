package orb

// signal_test.go covers the unit-level semantics that the end-to-end golden
// fixture does not exercise in isolation: config
// validation message anchoring, the symbol filter, the no-signal-in-range
// invariant, the two breakout-gate negatives, the stop selection branches,
// no-entry-after-EOD, session reset/flatten, state_dict round-trip, and the
// equity-pulled-at-sizing-time invariant.

import (
	"errors"
	"testing"
	"time"
)

func et(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load ET: %v", err)
	}
	return loc
}

func etBar(loc *time.Location, y int, mo time.Month, d, h, mi int, o, hi, lo, c float64, v int64) Bar {
	ts := time.Date(y, mo, d, h, mi, 0, 0, loc).UTC()
	return NewBarFromFloats("AAPL", ts, o, hi, lo, c, v)
}

func makeSG(t *testing.T, mutate func(*Config)) *Generator {
	t.Helper()
	cfg := DefaultConfig()
	cfg.Symbol = "AAPL"
	cfg.EquityProvider = func() float64 { return 100000 }
	if mutate != nil {
		mutate(&cfg)
	}
	g, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return g
}

// driveRange feeds `bars` opening-range bars at 5-min cadence from session_open.
func driveRange(t *testing.T, g *Generator, loc *time.Location, day, n int, hi, lo, c float64, v int64) {
	t.Helper()
	for i := 0; i < n; i++ {
		b := etBar(loc, 2024, time.January, day, 9, 30+i*5, c, hi, lo, c, v)
		if sigs := g.OnBar(b); len(sigs) != 0 {
			t.Fatalf("range bar %d emitted signals: %+v", i, sigs)
		}
	}
}

func TestConfigValidationMessages(t *testing.T) {
	provider := func() float64 { return 100000 }
	cases := []struct {
		name   string
		mutate func(*Config)
		substr string
	}{
		{"nil provider", func(c *Config) { c.EquityProvider = nil }, "equity_provider"},
		{"risk low", func(c *Config) { c.RiskPct = 0 }, "risk_pct"},
		{"risk high", func(c *Config) { c.RiskPct = 101 }, "risk_pct"},
		{"range zero", func(c *Config) { c.RangeMinutes = 0 }, "range_minutes"},
		{"vol zero", func(c *Config) { c.VolMultiple = 0 }, "vol_multiple"},
		{"ptr neg", func(c *Config) { c.ProfitTargetR = -1 }, "profit_target_r"},
		{"hard low", func(c *Config) { c.HardStopPct = 0 }, "hard_stop_pct"},
		{"hard high", func(c *Config) { c.HardStopPct = 75 }, "hard_stop_pct"},
		{"eod 25:00", func(c *Config) { c.EODExitTime = "25:00" }, "eod_exit_time"},
		{"eod noon", func(c *Config) { c.EODExitTime = "noon" }, "eod_exit_time"},
		{"bad tz", func(c *Config) { c.Timezone = "Not/A/Real_Timezone" }, "timezone"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.Symbol = "AAPL"
			cfg.EquityProvider = provider
			tc.mutate(&cfg)
			_, err := New(cfg)
			if err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
			if !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("error not ErrInvalidConfig: %v", err)
			}
			if !contains(err.Error(), tc.substr) {
				t.Fatalf("message %q lacks %q", err.Error(), tc.substr)
			}
		})
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestOutOfUniverseIgnored(t *testing.T) {
	g := makeSG(t, nil)
	loc := et(t)
	b := NewBarFromFloats("ZZZ", time.Date(2024, 1, 8, 9, 35, 0, 0, loc).UTC(), 100, 100, 100, 100, 1_000_000)
	if sigs := g.OnBar(b); sigs != nil {
		t.Fatalf("out-of-universe emitted %+v", sigs)
	}
	if g.currentSessionDate != nil {
		t.Fatalf("state changed for out-of-universe bar")
	}
	if g.lastSeenClose != nil {
		t.Fatalf("last_seen_close updated for out-of-universe bar")
	}
}

func TestNoSignalDuringRangeWindow(t *testing.T) {
	g := makeSG(t, nil)
	loc := et(t)
	driveRange(t, g, loc, 8, 6, 101.0, 99.0, 100.0, 1_000_000)
	if g.rangeHigh == nil || g.rangeHigh.String() != "101.0" {
		t.Fatalf("range_high = %v, want 101.0", g.rangeHigh)
	}
	if g.rangeLow == nil || g.rangeLow.String() != "99.0" {
		t.Fatalf("range_low = %v, want 99.0", g.rangeLow)
	}
	if g.rangeLocked {
		t.Fatalf("range locked prematurely")
	}
}

func TestNoEntryWeakVolume(t *testing.T) {
	g := makeSG(t, nil)
	loc := et(t)
	driveRange(t, g, loc, 8, 6, 101.0, 99.0, 100.0, 1_000_000)
	// close 102 > 101 but volume 1M not > 1M*1.5
	weak := etBar(loc, 2024, time.January, 8, 10, 5, 101.0, 102.0, 101.0, 102.0, 1_000_000)
	if sigs := g.OnBar(weak); sigs != nil {
		t.Fatalf("weak-vol breakout emitted %+v", sigs)
	}
	if g.positionQty != 0 {
		t.Fatalf("position opened on weak vol")
	}
	if !g.rangeLocked {
		t.Fatalf("range should have locked on post-range bar")
	}
}

func TestNoEntryPriceNotAboveRangeHigh(t *testing.T) {
	g := makeSG(t, nil)
	loc := et(t)
	driveRange(t, g, loc, 8, 6, 101.0, 99.0, 100.0, 1_000_000)
	// close == range_high (not strictly greater), big volume
	b := etBar(loc, 2024, time.January, 8, 10, 5, 100.5, 101.0, 100.0, 101.0, 5_000_000)
	if sigs := g.OnBar(b); sigs != nil {
		t.Fatalf("equal-close breakout emitted %+v", sigs)
	}
	if g.positionQty != 0 {
		t.Fatalf("position opened on equal close")
	}
}

func TestLongSignalSizing(t *testing.T) {
	g := makeSG(t, nil)
	loc := et(t)
	driveRange(t, g, loc, 8, 6, 101.0, 99.0, 100.0, 1_000_000)
	b := etBar(loc, 2024, time.January, 8, 10, 5, 101.0, 102.5, 101.0, 102.0, 2_000_000)
	sigs := g.OnBar(b)
	if len(sigs) != 1 || sigs[0].Side != SideLong {
		t.Fatalf("expected 1 LONG, got %+v", sigs)
	}
	if sigs[0].TargetQty != 980 {
		t.Fatalf("target_qty = %d, want 980", sigs[0].TargetQty)
	}
	if g.entryPrice.String() != "102.0" {
		t.Fatalf("entry = %s, want 102.0", g.entryPrice.String())
	}
	if g.stopPrice.String() != "100.980" {
		t.Fatalf("stop = %s, want 100.980", g.stopPrice.String())
	}
	if g.targetPrice.String() != "104.0400" {
		t.Fatalf("target = %s, want 104.0400", g.targetPrice.String())
	}
}

func TestStopUsesRangeLowWhenTighter(t *testing.T) {
	g := makeSG(t, func(c *Config) { c.HardStopPct = 5.0 })
	loc := et(t)
	driveRange(t, g, loc, 8, 6, 101.0, 99.0, 100.0, 1_000_000)
	b := etBar(loc, 2024, time.January, 8, 10, 5, 101.0, 102.5, 101.0, 102.0, 2_000_000)
	if sigs := g.OnBar(b); len(sigs) != 1 {
		t.Fatalf("expected entry, got %+v", sigs)
	}
	// range_low 99 wins over 102*0.95=96.9; reference keeps Decimal("99.0").
	if g.stopPrice.String() != "99.0" {
		t.Fatalf("stop = %s, want 99.0", g.stopPrice.String())
	}
}

func TestNoNewEntryAfterEOD(t *testing.T) {
	g := makeSG(t, nil)
	loc := et(t)
	driveRange(t, g, loc, 8, 6, 101.0, 99.0, 100.0, 1_000_000)
	// strong breakout exactly at 15:55 -> blocked
	b := etBar(loc, 2024, time.January, 8, 15, 55, 101.0, 102.5, 101.0, 102.0, 5_000_000)
	if sigs := g.OnBar(b); sigs != nil {
		t.Fatalf("entry after EOD emitted %+v", sigs)
	}
	if g.positionQty != 0 {
		t.Fatalf("position opened after EOD")
	}
}

func TestSessionChangeFlattensLingering(t *testing.T) {
	g := makeSG(t, nil)
	loc := et(t)
	// Day 1 entry, no exit.
	driveRange(t, g, loc, 8, 6, 101.0, 99.0, 100.0, 1_000_000)
	b := etBar(loc, 2024, time.January, 8, 10, 5, 101.0, 102.5, 101.0, 102.0, 2_000_000)
	if sigs := g.OnBar(b); len(sigs) != 1 {
		t.Fatalf("day1 entry failed: %+v", sigs)
	}
	held := g.positionQty
	// Day 2 first bar flushes the lingering position.
	d2 := etBar(loc, 2024, time.January, 9, 9, 30, 200.0, 201.0, 199.0, 200.0, 1_000_000)
	sigs := g.OnBar(d2)
	var flat *Signal
	for k := range sigs {
		if sigs[k].Side == SideFlat {
			flat = &sigs[k]
		}
	}
	if flat == nil {
		t.Fatalf("no FLAT on session boundary: %+v", sigs)
	}
	if flat.TargetQty != held {
		t.Fatalf("FLAT qty = %d, want %d", flat.TargetQty, held)
	}
	if !contains(flat.Reason, "session") {
		t.Fatalf("FLAT reason = %q", flat.Reason)
	}
	if g.positionQty != 0 {
		t.Fatalf("position not cleared after boundary flush")
	}
}

func TestStateDictRoundTrip(t *testing.T) {
	g := makeSG(t, nil)
	loc := et(t)
	driveRange(t, g, loc, 8, 6, 101.0, 99.0, 100.0, 1_000_000)
	b := etBar(loc, 2024, time.January, 8, 10, 5, 101.0, 102.5, 101.0, 102.0, 2_000_000)
	g.OnBar(b)

	snap := g.StateDict()
	g2 := makeSG(t, nil)
	if err := g2.LoadState(snap); err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if g2.positionQty != g.positionQty ||
		g2.entryPrice.String() != g.entryPrice.String() ||
		g2.stopPrice.String() != g.stopPrice.String() ||
		g2.targetPrice.String() != g.targetPrice.String() ||
		g2.rangeLocked != g.rangeLocked ||
		g2.rangeTotalVolume != g.rangeTotalVolume ||
		g2.avgVolume != g.avgVolume {
		t.Fatalf("round-trip mismatch:\n g=%+v\ng2=%+v", g.StateDict(), g2.StateDict())
	}
	if g2.currentSessionDate == nil || !g2.currentSessionDate.equal(*g.currentSessionDate) {
		t.Fatalf("session date not restored")
	}
}

func TestStateDictColdStartRoundTrip(t *testing.T) {
	g := makeSG(t, nil)
	snap := g.StateDict()
	if snap.EntryPrice != "0" || snap.StopPrice != "0" || snap.TargetPrice != "0" {
		t.Fatalf("cold-start prices should be \"0\", got %q/%q/%q", snap.EntryPrice, snap.StopPrice, snap.TargetPrice)
	}
	if snap.CurrentSessionDate != nil {
		t.Fatalf("cold-start session date should be null")
	}
	if snap.Config.EquityAtSnapshot != 100000 {
		t.Fatalf("equity_at_snapshot = %v", snap.Config.EquityAtSnapshot)
	}
	g2 := makeSG(t, nil)
	if err := g2.LoadState(snap); err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if g2.positionQty != 0 || g2.currentSessionDate != nil || g2.rangeHigh != nil || g2.rangeLocked {
		t.Fatalf("cold-start round-trip dirtied state")
	}
}

func TestEquityPulledAtSizingTime(t *testing.T) {
	loc := et(t)
	enter := func(equity float64) int {
		g := makeSG(t, func(c *Config) { c.EquityProvider = func() float64 { return equity } })
		driveRange(t, g, loc, 8, 6, 101.0, 99.0, 100.0, 1_000_000)
		b := etBar(loc, 2024, time.January, 8, 10, 5, 101.0, 102.5, 101.0, 102.0, 2_000_000)
		g.OnBar(b)
		return g.positionQty
	}
	small := enter(100000)
	if small != 980 {
		t.Fatalf("100k equity -> %d shares, want 980", small)
	}
	large := enter(400000)
	expected := 4 * small
	drift := large - expected
	if drift < 0 {
		drift = -drift
	}
	tol := expected / 100
	if tol < 2 {
		tol = 2
	}
	if drift > tol {
		t.Fatalf("400k equity -> %d shares, want ~%d", large, expected)
	}
}

func TestIntentGenerationMonotonic(t *testing.T) {
	g := makeSG(t, nil)
	g1 := g.EvaluateSignal(time.Date(2025, 1, 6, 14, 0, 0, 0, time.UTC)).Generation
	g2 := g.EvaluateSignal(time.Date(2025, 1, 6, 14, 1, 0, 0, time.UTC)).Generation
	if !(g2 > g1) {
		t.Fatalf("generation not monotonic: %d -> %d", g1, g2)
	}
}

func TestIntentStatesDirect(t *testing.T) {
	// Mirrors test_intent.py: drive internal fields directly.
	loc := et(t)
	newSG := func() *Generator {
		return makeSG(t, func(c *Config) {
			c.Symbol = "SPY"
			c.RiskPct = 0.5
		})
	}
	asOf := time.Date(2025, 1, 6, 15, 0, 0, 0, time.UTC)
	sessDate := civilDate{2025, time.January, 6}

	// NO_SETUP before range locks.
	if it := newSG().EvaluateSignal(time.Date(2025, 1, 6, 14, 0, 0, 0, time.UTC)); it.State != StateNoSetup || it.ORBHigh != "" {
		t.Fatalf("expected NO_SETUP/nil orb_high, got %+v", it)
	}

	// FORMING when locked and flat.
	g := newSG()
	rh := mustDec("500")
	rl := mustDec("498")
	g.rangeHigh, g.rangeLow, g.rangeLocked = &rh, &rl, true
	g.currentSessionDate = &sessDate
	if it := g.EvaluateSignal(asOf); it.State != StateForming || it.ORBHigh != "500" || it.ORBLow != "498" {
		t.Fatalf("expected FORMING, got %+v", it)
	}

	// BUY when close above orb_high in window.
	g = newSG()
	g.rangeHigh, g.rangeLow, g.rangeLocked = &rh, &rl, true
	g.currentSessionDate = &sessDate
	lc := mustDec("500.50")
	g.lastSeenClose = &lc
	if it := g.EvaluateSignal(asOf); it.State != StateBuy || it.ProximityToTriggerPct == nil || *it.ProximityToTriggerPct <= 0 {
		t.Fatalf("expected BUY with positive proximity, got %+v", it)
	}

	// HOLD when in position.
	g = newSG()
	g.rangeHigh, g.rangeLow, g.rangeLocked = &rh, &rl, true
	g.positionQty = 100
	g.currentSessionDate = &sessDate
	if it := g.EvaluateSignal(asOf); it.State != StateHold {
		t.Fatalf("expected HOLD, got %+v", it)
	}

	// NO_SETUP after EOD window closed (21:00 UTC > 15:55 ET).
	g = newSG()
	g.rangeHigh, g.rangeLow, g.rangeLocked = &rh, &rl, true
	g.currentSessionDate = &sessDate
	after := time.Date(2025, 1, 6, 21, 0, 0, 0, time.UTC)
	if it := g.EvaluateSignal(after); it.State != StateNoSetup {
		t.Fatalf("expected NO_SETUP post-EOD, got %+v", it)
	}
	_ = loc
}
