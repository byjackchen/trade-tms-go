package runner

import (
	"math"
	"testing"

	"github.com/byjackchen/trade-tms-go/internal/indicators"
)

// TestRecomputeReadiness covers the RS-stamping pass's pure JSONB-recompute
// helper: given a decoded intent payload (json numbers are float64) and the
// now-known RS rank, it must reproduce indicators.BuyReadiness from the payload's
// own proximity/risk/base facts, and skip rows lacking those facts.
func TestRecomputeReadiness(t *testing.T) {
	// A forming row with full trade-plan facts + a VCP base.
	intent := map[string]any{
		"proximity_to_trigger_pct": 1.0,
		"risk_pct":                 4.0,
		"base_depth_pct":           6.0,
	}
	got, ok := recomputeReadiness(intent, 95)
	if !ok {
		t.Fatal("expected recompute to succeed for a full trade-plan row")
	}
	want := indicators.BuyReadiness(indicators.BuyReadinessInputs{
		ProximityPct: 1.0, RSRank: 95, HasVCP: true, BaseDepthPct: 6.0, RiskPct: 4.0,
	})
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("recomputeReadiness = %v, want %v", got, want)
	}

	// No VCP base (swing fallback): base_depth_pct absent -> HasVCP false.
	noVCP := map[string]any{
		"proximity_to_trigger_pct": 2.0,
		"risk_pct":                 5.0,
		"base_depth_pct":           nil,
	}
	g2, ok2 := recomputeReadiness(noVCP, 80)
	if !ok2 {
		t.Fatal("expected recompute to succeed without a VCP base")
	}
	w2 := indicators.BuyReadiness(indicators.BuyReadinessInputs{
		ProximityPct: 2.0, RSRank: 80, HasVCP: false, RiskPct: 5.0,
	})
	if math.Abs(g2-w2) > 1e-9 {
		t.Fatalf("recomputeReadiness(noVCP) = %v, want %v", g2, w2)
	}

	// A no_setup-style row missing proximity/risk -> ok=false (leave readiness).
	if _, ok := recomputeReadiness(map[string]any{}, 50); ok {
		t.Fatal("expected recompute to skip a row without trade-plan facts")
	}
}

func TestJSONFloat(t *testing.T) {
	if v, ok := jsonFloat(float64(3.5)); !ok || v != 3.5 {
		t.Fatalf("jsonFloat(float64) = %v,%v", v, ok)
	}
	if _, ok := jsonFloat(nil); ok {
		t.Fatal("jsonFloat(nil) should be !ok")
	}
	if _, ok := jsonFloat("x"); ok {
		t.Fatal("jsonFloat(string) should be !ok")
	}
}
