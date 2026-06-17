package metrics

import (
	"encoding/json"
	"os"
	"testing"
)

// TestGolden252PointCurve pins the Neumaier-compensated metrics to <=1e-12
// relative on a 252-point randomized walk (spec §13.1 — the case where naive Go
// summation can drift in the last ulp). The curve and the expected outputs are
// this repo's golden regression baseline; any drift is a regression.
func TestGolden252PointCurve(t *testing.T) {
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
