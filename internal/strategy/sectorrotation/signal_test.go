package sectorrotation

import (
	"strings"
	"testing"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/domain"
)

var smallUniverse = []string{"AAA", "BBB", "CCC", "DDD"}

func mkSG(t *testing.T, lookback, topK int, equity float64) *SignalGenerator {
	t.Helper()
	sg, err := New(Config{
		EquityProvider:   func() float64 { return equity },
		Universe:         smallUniverse,
		MomentumLookback: lookback,
		TopK:             topK,
		Timezone:         "America/New_York",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return sg
}

func bar(sym string, ts time.Time, close float64) domain.Bar {
	p, err := domain.PriceFromFloat64(close)
	if err != nil {
		panic(err)
	}
	return domain.Bar{Symbol: sym, TS: ts, Open: p, High: p, Low: p, Close: p, Volume: 1_000_000}
}

// driveDays replays `days` daily bars across all universe symbols in canonical
// order.
func driveDays(sg *SignalGenerator, start time.Time, days int, closes map[string][]float64) []domain.Signal {
	var out []domain.Signal
	for day := 0; day < days; day++ {
		ts := start.AddDate(0, 0, day)
		for _, sym := range sg.cfg.Universe {
			cs := closes[sym]
			if day >= len(cs) {
				continue
			}
			out = append(out, sg.OnBar(bar(sym, ts, cs[day]))...)
		}
	}
	return out
}

func ramp(base, step float64, n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = base + float64(i)*step
	}
	return out
}

func TestDefaultUniverseHasElevenETFs(t *testing.T) {
	if len(DefaultUniverse) != 11 {
		t.Fatalf("DefaultUniverse len = %d, want 11", len(DefaultUniverse))
	}
	has := func(s string) bool {
		for _, x := range DefaultUniverse {
			if x == s {
				return true
			}
		}
		return false
	}
	if !has("XLK") || !has("XLF") {
		t.Errorf("DefaultUniverse missing XLK/XLF")
	}
}

func TestConfigValidation(t *testing.T) {
	base := Config{EquityProvider: func() float64 { return 1 }, Universe: smallUniverse, MomentumLookback: 5, TopK: 2, Timezone: "x"}
	if err := base.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	bad := base
	bad.EquityProvider = nil
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "equity_provider") {
		t.Errorf("nil equity_provider: %v", err)
	}
	bad = base
	bad.Universe = nil
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "universe") {
		t.Errorf("empty universe: %v", err)
	}
	bad = base
	bad.MomentumLookback = 1
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "momentum_lookback") {
		t.Errorf("lookback<2: %v", err)
	}
	for _, k := range []int{0, 99} {
		bad = base
		bad.TopK = k
		if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "top_k") {
			t.Errorf("top_k=%d: %v", k, err)
		}
	}
	// top_k message embeds len(universe).
	bad = base
	bad.TopK = 99
	if err := bad.Validate(); !strings.Contains(err.Error(), "top_k must be in [1, 4], got 99") {
		t.Errorf("top_k message = %v", err)
	}
}

func TestOutOfUniverseSymbolIgnored(t *testing.T) {
	sg := mkSG(t, 5, 2, 100000)
	ts := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	if got := sg.OnBar(bar("ZZZ", ts, 100)); got != nil {
		t.Errorf("out-of-universe produced %v", got)
	}
	if _, ok := sg.history["ZZZ"]; ok {
		t.Errorf("ZZZ leaked into history")
	}
}

func TestWarmupNoSignalWithinFirstMonth(t *testing.T) {
	sg := mkSG(t, 5, 2, 100000)
	closes := map[string][]float64{}
	for _, s := range smallUniverse {
		closes[s] = ramp(100, 1, 10)
	}
	out := driveDays(sg, time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC), 10, closes)
	if len(out) != 0 {
		t.Errorf("expected no signals within first month, got %v", out)
	}
}

func TestNoSignalOnRolloverIfWarmupIncomplete(t *testing.T) {
	sg := mkSG(t, 20, 2, 100000)
	closes := map[string][]float64{}
	for _, s := range smallUniverse {
		closes[s] = ramp(100, 1, 8)
	}
	out := driveDays(sg, time.Date(2024, 1, 24, 0, 0, 0, 0, time.UTC), 8, closes)
	if len(out) != 0 {
		t.Errorf("expected no signals (under warmup), got %v", out)
	}
}

func TestRebalanceFiresOnFirstBarOfNewMonth(t *testing.T) {
	sg := mkSG(t, 20, 2, 100000)
	closes := map[string][]float64{
		"AAA": ramp(100, 1.0, 30),
		"BBB": ramp(100, 0.5, 30),
		"CCC": ramp(100, 0, 30),
		"DDD": ramp(100, -0.5, 30),
	}
	out := driveDays(sg, time.Date(2024, 1, 4, 0, 0, 0, 0, time.UTC), 30, closes)
	var longs, flats []domain.Signal
	for _, s := range out {
		switch s.Side {
		case domain.SideLong:
			longs = append(longs, s)
		case domain.SideFlat:
			flats = append(flats, s)
		}
	}
	if len(longs) != 2 || len(flats) != 0 {
		t.Fatalf("longs=%d flats=%d", len(longs), len(flats))
	}
	got := map[string]bool{longs[0].Symbol: true, longs[1].Symbol: true}
	if !got["AAA"] || !got["BBB"] {
		t.Errorf("bought = %v, want AAA+BBB", got)
	}
}

func TestNoRebalanceWithinSameMonth(t *testing.T) {
	sg := mkSG(t, 20, 2, 100000)
	closes := map[string][]float64{
		"AAA": ramp(100, 1.0, 40),
		"BBB": ramp(100, 0.5, 40),
		"CCC": ramp(100, 0, 40),
		"DDD": ramp(100, -0.5, 40),
	}
	out := driveDays(sg, time.Date(2024, 1, 4, 0, 0, 0, 0, time.UTC), 40, closes)
	dates := map[string]struct{}{}
	for _, s := range out {
		dates[s.TS.Format("2006-01-02")] = struct{}{}
	}
	if len(dates) != 1 {
		t.Errorf("expected exactly one rebalance date, got %v", dates)
	}
}

func TestRebalanceEmitsFlatForDroppedHoldings(t *testing.T) {
	sg := mkSG(t, 20, 2, 100000)
	driveDays(sg, time.Date(2024, 1, 4, 0, 0, 0, 0, time.UTC), 30, map[string][]float64{
		"AAA": ramp(100, 1.0, 30), "BBB": ramp(100, 0.5, 30),
		"CCC": ramp(100, 0, 30), "DDD": ramp(100, -0.5, 30),
	})
	if sg.currentPositions["AAA"] <= 0 || sg.currentPositions["BBB"] <= 0 {
		t.Fatalf("phase1 positions not established")
	}
	out := driveDays(sg, time.Date(2024, 2, 3, 0, 0, 0, 0, time.UTC), 30, map[string][]float64{
		"AAA": ramp(129, -0.5, 30), "BBB": ramp(114.5, -0.3, 30),
		"CCC": ramp(100, 1.5, 30), "DDD": ramp(85.5, 1.0, 30),
	})
	flats, longs := map[string]bool{}, map[string]bool{}
	for _, s := range out {
		if s.Side == domain.SideFlat {
			flats[s.Symbol] = true
		} else if s.Side == domain.SideLong {
			longs[s.Symbol] = true
		}
	}
	if !flats["AAA"] || !flats["BBB"] || len(flats) != 2 {
		t.Errorf("flats=%v want AAA+BBB", flats)
	}
	if !longs["CCC"] || !longs["DDD"] || len(longs) != 2 {
		t.Errorf("longs=%v want CCC+DDD", longs)
	}
}

func TestHeldPositionStaysIfStillInTopK(t *testing.T) {
	sg := mkSG(t, 20, 2, 100000)
	driveDays(sg, time.Date(2024, 1, 4, 0, 0, 0, 0, time.UTC), 30, map[string][]float64{
		"AAA": ramp(100, 1.0, 30), "BBB": ramp(100, 0.5, 30),
		"CCC": ramp(100, 0, 30), "DDD": ramp(100, -0.5, 30),
	})
	aaaQty := sg.currentPositions["AAA"]
	out := driveDays(sg, time.Date(2024, 2, 3, 0, 0, 0, 0, time.UTC), 30, map[string][]float64{
		"AAA": ramp(129, 1.0, 30), "BBB": ramp(114.5, 0, 30),
		"CCC": ramp(100, 0.6, 30), "DDD": ramp(85.5, 0, 30),
	})
	for _, s := range out {
		if s.Symbol == "AAA" {
			t.Errorf("AAA churned: %+v", s)
		}
	}
	if sg.currentPositions["AAA"] != aaaQty {
		t.Errorf("AAA qty changed %d -> %d", aaaQty, sg.currentPositions["AAA"])
	}
}

func TestTopKEqualWeightShareCount(t *testing.T) {
	sg := mkSG(t, 20, 2, 100000)
	closes := map[string][]float64{
		"AAA": ramp(100, 1.0, 30), // day 27 close = 127
		"BBB": ramp(50, 0.5, 30),  // day 27 close = 63.5
		"CCC": ramp(100, 0, 30),
		"DDD": ramp(100, -0.5, 30),
	}
	out := driveDays(sg, time.Date(2024, 1, 4, 0, 0, 0, 0, time.UTC), 30, closes)
	longs := map[string]domain.Signal{}
	for _, s := range out {
		if s.Side == domain.SideLong {
			longs[s.Symbol] = s
		}
	}
	// Rebalance on Feb 1 uses Jan 31 closes (index 27): 50000//127 and 50000//63.5.
	wantAAA := int64(50000) / int64(127) // 393 (floor of 50000/127.0)
	if int64(longs["AAA"].TargetQty) != wantAAA {
		t.Errorf("AAA qty = %d, want %d", longs["AAA"].TargetQty, wantAAA)
	}
	// 50000 // 63.5 = 787
	if int64(longs["BBB"].TargetQty) != 787 {
		t.Errorf("BBB qty = %d, want 787", longs["BBB"].TargetQty)
	}
}

func TestRebalanceSizingPullsLiveEquity(t *testing.T) {
	equity := 100000.0
	closes := map[string][]float64{
		"AAA": ramp(100, 1.0, 30), "BBB": ramp(50, 0.5, 30),
		"CCC": ramp(100, 0, 30), "DDD": ramp(100, -0.5, 30),
	}
	run := func(eq float64) map[string]int64 {
		sg, _ := New(Config{EquityProvider: func() float64 { return eq }, Universe: smallUniverse, MomentumLookback: 20, TopK: 2, Timezone: "x"})
		out := driveDays(sg, time.Date(2024, 1, 4, 0, 0, 0, 0, time.UTC), 30, closes)
		m := map[string]int64{}
		for _, s := range out {
			if s.Side == domain.SideLong {
				m[s.Symbol] = int64(s.TargetQty)
			}
		}
		return m
	}
	small := run(equity)
	large := run(equity * 4)
	for sym, q := range small {
		exp := 4 * q
		tol := exp / 100
		if tol < 2 {
			tol = 2
		}
		d := large[sym] - exp
		if d < 0 {
			d = -d
		}
		if d > tol {
			t.Errorf("%s: 4x equity -> %d, want ~%d", sym, large[sym], exp)
		}
	}
}

func TestReasonStringFormat(t *testing.T) {
	sg := mkSG(t, 20, 2, 100000)
	out := driveDays(sg, time.Date(2024, 1, 4, 0, 0, 0, 0, time.UTC), 30, map[string][]float64{
		"AAA": ramp(100, 1.0, 30), "BBB": ramp(100, 0.5, 30),
		"CCC": ramp(100, 0, 30), "DDD": ramp(100, -0.5, 30),
	})
	for _, s := range out {
		if s.Side == domain.SideLong && s.Symbol == "AAA" {
			// Deque holds the last 21 closes; at the Feb-1 rebalance the front
			// is Jan-11's close (index 7 = 107) and the back is Jan-31's
			// (index 27 = 127): (127-107)/107 = 0.186915... -> +18.69%.
			want := "Sector Rotation rebalance :: top-2 entry, 20-bar return +18.69%"
			if s.Reason != want {
				t.Errorf("LONG reason = %q, want %q", s.Reason, want)
			}
		}
	}
}

func TestStateDictRoundTrip(t *testing.T) {
	sg := mkSG(t, 10, 2, 100000)
	closes := map[string][]float64{
		"AAA": ramp(100, 1.0, 35), "BBB": ramp(100, 0.5, 35),
		"CCC": ramp(100, 0, 35), "DDD": ramp(100, -0.5, 35),
	}
	driveDays(sg, time.Date(2024, 1, 4, 0, 0, 0, 0, time.UTC), 35, closes)
	snap := sg.StateDict()
	sg2 := mkSG(t, 10, 2, 100000)
	if err := sg2.LoadState(snap); err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	for _, sym := range smallUniverse {
		if sg2.currentPositions[sym] != sg.currentPositions[sym] {
			t.Errorf("%s pos %d != %d", sym, sg2.currentPositions[sym], sg.currentPositions[sym])
		}
		a, b := sg.history[sym].snapshot(), sg2.history[sym].snapshot()
		if len(a) != len(b) {
			t.Errorf("%s history len %d != %d", sym, len(a), len(b))
			continue
		}
		for i := range a {
			if a[i] != b[i] {
				t.Errorf("%s history[%d] %v != %v", sym, i, a[i], b[i])
			}
		}
	}
	if (sg.lastUniverseDate == nil) != (sg2.lastUniverseDate == nil) {
		t.Errorf("lastUniverseDate nil mismatch")
	} else if sg.lastUniverseDate != nil && !sg.lastUniverseDate.Equal(*sg2.lastUniverseDate) {
		t.Errorf("lastUniverseDate %v != %v", sg.lastUniverseDate, sg2.lastUniverseDate)
	}
}

func TestStateDictRecordsEquitySnapshot(t *testing.T) {
	sg := mkSG(t, 20, 2, 250000)
	snap := sg.StateDict()
	if snap.Config.EquityAtSnapshot != 250000.0 {
		t.Errorf("equity_at_snapshot = %v", snap.Config.EquityAtSnapshot)
	}
}

func TestStateSummaryColdStart(t *testing.T) {
	sg := mkSG(t, 20, 2, 100000)
	s := sg.StateSummary()
	if len(s.CurrentHoldings) != 0 {
		t.Errorf("holdings = %v", s.CurrentHoldings)
	}
	if s.LastUniverseDate != nil {
		t.Errorf("last date = %v", *s.LastUniverseDate)
	}
	if s.TopK != 2 || s.UniverseSize != 4 {
		t.Errorf("top_k=%d universe_size=%d", s.TopK, s.UniverseSize)
	}
}

func TestStateSummaryAfterRebalance(t *testing.T) {
	sg := mkSG(t, 20, 2, 100000)
	driveDays(sg, time.Date(2024, 1, 4, 0, 0, 0, 0, time.UTC), 30, map[string][]float64{
		"AAA": ramp(100, 1.0, 30), "BBB": ramp(100, 0.5, 30),
		"CCC": ramp(100, 0, 30), "DDD": ramp(100, -0.5, 30),
	})
	s := sg.StateSummary()
	if len(s.CurrentHoldings) != 2 || s.CurrentHoldings["AAA"] <= 0 || s.CurrentHoldings["BBB"] <= 0 {
		t.Errorf("holdings = %v", s.CurrentHoldings)
	}
	if s.LastUniverseDate == nil || !strings.HasPrefix(*s.LastUniverseDate, "2024-") {
		t.Errorf("last date = %v", s.LastUniverseDate)
	}
}
