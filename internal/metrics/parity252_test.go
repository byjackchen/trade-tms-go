package metrics

import (
	"encoding/json"
	"os"
	"testing"
)

// TestParity252PointCurve asserts the Neumaier-compensated metrics agree with
// the Python reference to <=1e-12 relative on a 252-point randomized walk
// (spec §13.1 — the case where naive Go summation can drift in the last ulp).
// The curve and the reference outputs were captured from the reference CPython
// venv (research.metrics over the same seeded walk).
func TestParity252PointCurve(t *testing.T) {
	raw, err := os.ReadFile("testdata/curve252.json")
	if err != nil {
		t.Fatalf("read curve: %v", err)
	}
	var curve []float64
	if err := json.Unmarshal(raw, &curve); err != nil {
		t.Fatalf("decode curve: %v", err)
	}
	if len(curve) != 252 {
		t.Fatalf("expected 252 points, got %d", len(curve))
	}
	relClose(t, "sharpe", Sharpe(curve), 1.059069716952741)
	relClose(t, "calmar", Calmar(curve), 1.5831402501143759)
	relClose(t, "mdd", MaxDrawdownPct(curve), -12.366693406157973)
}
