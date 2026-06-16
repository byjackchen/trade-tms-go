package indicators

import (
	"math"
	"testing"
)

// Tests for the TMS-enhancement actionable trade-plan + cross-sectional RS-rank
// primitives (relstrength.go, tradeplan.go). These are NOT in the Python SEPA
// reference; the parity golden test covers the diverged intent wiring separately.

func TestSwingHighPivotAndLowStop(t *testing.T) {
	high := []float64{10, 11, 9, 13, 12, 8, 14, 10, 11, 12, 99}
	low := []float64{8, 7, 6, 9, 8, 5, 10, 7, 8, 9, 50}

	// Include the latest (completedExclusive=false): last 10 bars are indices 1..10.
	// Highest high in {11,9,13,12,8,14,10,11,12,99} = 99.
	if got := SwingHighPivot(high, 10, false); got != 99 {
		t.Fatalf("SwingHighPivot incl-latest = %v, want 99", got)
	}
	// Lowest low in indices 1..10 {7,6,9,8,5,10,7,8,9,50} = 5.
	if got := SwingLowStop(low, 10, false); got != 5 {
		t.Fatalf("SwingLowStop incl-latest = %v, want 5", got)
	}

	// Exclude the forming bar (completedExclusive=true): drop index 10 (99/50),
	// last 10 completed bars are indices 0..9; highest = 14, lowest = 5.
	if got := SwingHighPivot(high, 10, true); got != 14 {
		t.Fatalf("SwingHighPivot excl-latest = %v, want 14", got)
	}
	if got := SwingLowStop(low, 10, true); got != 5 {
		t.Fatalf("SwingLowStop excl-latest = %v, want 5", got)
	}

	// Empty history -> 0 (caller guards keep the plan non-null another way).
	if got := SwingHighPivot(nil, 10, false); got != 0 {
		t.Fatalf("SwingHighPivot(empty) = %v, want 0", got)
	}
}

func TestProximityToTriggerPctSign(t *testing.T) {
	// Price BELOW pivot -> positive proximity (approaching the buy point).
	if got := ProximityToTriggerPct(110, 100); math.Abs(got-10.0) > 1e-9 {
		t.Fatalf("proximity below pivot = %v, want +10", got)
	}
	// Price ABOVE pivot (extended) -> negative proximity.
	if got := ProximityToTriggerPct(100, 110); got >= 0 {
		t.Fatalf("proximity above pivot = %v, want negative", got)
	}
	// Exactly at pivot -> 0.
	if got := ProximityToTriggerPct(100, 100); got != 0 {
		t.Fatalf("proximity at pivot = %v, want 0", got)
	}
}

func TestRiskAndPctOff52wk(t *testing.T) {
	if got := RiskPct(100, 92); math.Abs(got-8.0) > 1e-9 {
		t.Fatalf("RiskPct = %v, want 8", got)
	}
	// pct off 52wk high is <= 0; 0 = at a new high.
	if got := PctOff52wkHigh(90, 100); math.Abs(got-(-10.0)) > 1e-9 {
		t.Fatalf("PctOff52wkHigh = %v, want -10", got)
	}
	if got := PctOff52wkHigh(100, 100); got != 0 {
		t.Fatalf("PctOff52wkHigh at high = %v, want 0", got)
	}
}

func TestVolumeRatio(t *testing.T) {
	vol := make([]float64, 50)
	for i := range vol {
		vol[i] = 100
	}
	vol[49] = 250 // today 2.5x the 50-bar avg... but avg includes today.
	// avg of 49*100 + 250 = (4900+250)/50 = 103; ratio = 250/103.
	got := VolumeRatio(vol, 50)
	want := 250.0 / ((49*100.0 + 250.0) / 50.0)
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("VolumeRatio = %v, want %v", got, want)
	}
	// Insufficient history -> 0.
	if got := VolumeRatio(vol[:10], 50); got != 0 {
		t.Fatalf("VolumeRatio(short) = %v, want 0", got)
	}
}

func TestBuyReadinessOrdering(t *testing.T) {
	// A: leader sitting just under its pivot, tight VCP, low risk -> high score.
	a := BuyReadiness(BuyReadinessInputs{ProximityPct: 1.0, RSRank: 95, HasVCP: true, BaseDepthPct: 6, RiskPct: 4})
	// B: same but far below pivot -> lower (no near-term trigger).
	b := BuyReadiness(BuyReadinessInputs{ProximityPct: 20.0, RSRank: 95, HasVCP: true, BaseDepthPct: 6, RiskPct: 4})
	// C: extended ABOVE pivot -> lower than A (mostly already triggered).
	c := BuyReadiness(BuyReadinessInputs{ProximityPct: -4.0, RSRank: 95, HasVCP: true, BaseDepthPct: 6, RiskPct: 4})
	// D: same proximity as A but weak RS -> lower than A.
	d := BuyReadiness(BuyReadinessInputs{ProximityPct: 1.0, RSRank: 10, HasVCP: true, BaseDepthPct: 6, RiskPct: 4})
	// E: same as A but no VCP (swing fallback, looser base) -> lower than A.
	e := BuyReadiness(BuyReadinessInputs{ProximityPct: 1.0, RSRank: 95, HasVCP: false, RiskPct: 4})

	if !(a > b) {
		t.Errorf("near-pivot (%v) should beat far-below (%v)", a, b)
	}
	if !(a > c) {
		t.Errorf("near-pivot (%v) should beat extended (%v)", a, c)
	}
	if !(a > d) {
		t.Errorf("high-RS (%v) should beat low-RS (%v)", a, d)
	}
	if !(a > e) {
		t.Errorf("VCP base (%v) should beat swing fallback (%v)", a, e)
	}
	for _, s := range []float64{a, b, c, d, e} {
		if s < 0 || s > 100 {
			t.Errorf("readiness %v out of [0,100]", s)
		}
	}
}

func TestRSRawScoreSkipsShortHistory(t *testing.T) {
	short := make([]float64, RSLookback252) // need > 252 bars
	for i := range short {
		short[i] = 100
	}
	if _, ok := RSRawScore(short); ok {
		t.Fatalf("RSRawScore should skip a series with only 252 bars")
	}

	// Full history, monotonically rising -> positive blended score.
	full := make([]float64, RSLookback252+1)
	for i := range full {
		full[i] = 100.0 + float64(i)
	}
	score, ok := RSRawScore(full)
	if !ok {
		t.Fatalf("RSRawScore should accept a full-history series")
	}
	if score <= 0 {
		t.Fatalf("rising series RS score = %v, want > 0", score)
	}
}

func TestRSRankUniversePercentile(t *testing.T) {
	// Five symbols with strictly increasing raw scores: the strongest -> 99,
	// the weakest -> 1, the median -> 50.
	raw := map[string]float64{
		"E": 5.0, // strongest
		"D": 4.0,
		"C": 3.0, // median
		"B": 2.0,
		"A": 1.0, // weakest
	}
	ranks := RSRankUniverse(raw)
	if ranks["E"] != RSRankMax {
		t.Errorf("strongest rank = %d, want %d", ranks["E"], RSRankMax)
	}
	if ranks["A"] != RSRankMin {
		t.Errorf("weakest rank = %d, want %d", ranks["A"], RSRankMin)
	}
	if ranks["C"] != 50 {
		t.Errorf("median rank = %d, want 50", ranks["C"])
	}
	// Monotonic: higher raw -> higher-or-equal rank.
	if !(ranks["A"] <= ranks["B"] && ranks["B"] <= ranks["C"] &&
		ranks["C"] <= ranks["D"] && ranks["D"] <= ranks["E"]) {
		t.Errorf("ranks not monotonic: %+v", ranks)
	}

	// Ties share a rank.
	tie := map[string]float64{"X": 1.0, "Y": 1.0, "Z": 9.0}
	tr := RSRankUniverse(tie)
	if tr["X"] != tr["Y"] {
		t.Errorf("tied symbols got different ranks: X=%d Y=%d", tr["X"], tr["Y"])
	}

	// Single-symbol universe is trivially the strongest.
	one := RSRankUniverse(map[string]float64{"solo": 0.0})
	if one["solo"] != RSRankMax {
		t.Errorf("single-symbol rank = %d, want %d", one["solo"], RSRankMax)
	}

	// Empty universe -> empty map.
	if len(RSRankUniverse(nil)) != 0 {
		t.Errorf("empty universe should yield empty ranks")
	}
}
