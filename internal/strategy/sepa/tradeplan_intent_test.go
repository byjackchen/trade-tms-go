package sepa

import "testing"

// TestFormingIntentCarriesTradePlan proves the TMS-enhancement contract: a SEPA
// forming signal NEVER leaves the actionable trade-plan fields null. The
// no_breakout fixture drives a trend-template-passing name to just under its
// pivot (forming), where only strength would otherwise be carried — the
// generator must additionally carry pivot/stop/proximity/risk/%off-52wk/vol_ratio/
// buy_readiness. (The golden test covers the sanctioned divergence separately.)
func TestFormingIntentCarriesTradePlan(t *testing.T) {
	g := mkSG(t, sgOpt{regime: "bull"})
	bars := noBreakoutBars()
	for _, b := range bars {
		g.OnBar(b)
	}
	last := bars[len(bars)-1]
	it := g.EvaluateSignal(last.TS)

	if it.State != StateForming {
		t.Fatalf("expected forming state, got %q", it.State)
	}

	// Every actionable trade-plan field must be non-null for a forming signal.
	if it.PivotPrice == "" {
		t.Error("forming intent missing pivot_price")
	}
	if it.StopPrice == "" {
		t.Error("forming intent missing stop_price")
	}
	if it.ProximityToTriggerP == nil {
		t.Fatal("forming intent missing proximity_to_trigger_pct")
	}
	if it.RiskPct == nil {
		t.Fatal("forming intent missing risk_pct")
	}
	if it.PctOff52wkH == nil {
		t.Fatal("forming intent missing pct_off_52wk_high")
	}
	if it.VolRatio == nil {
		t.Fatal("forming intent missing vol_ratio")
	}
	if it.BuyReadiness == nil {
		t.Fatal("forming intent missing buy_readiness")
	}

	// Pivot > 0 and stop strictly in (0, pivot) — the invariant the swing fallback
	// guarantees.
	pivot := parsePyFloat(it.PivotPrice)
	stop := parsePyFloat(it.StopPrice)
	if pivot <= 0 {
		t.Errorf("pivot %v must be > 0", pivot)
	}
	if stop <= 0 || stop >= pivot {
		t.Errorf("stop %v must be in (0, pivot=%v)", stop, pivot)
	}

	// risk_pct = (pivot-stop)/pivot*100 must be positive.
	if *it.RiskPct <= 0 {
		t.Errorf("risk_pct %v must be > 0", *it.RiskPct)
	}
	// pct_off_52wk_high must be <= 0 (0 = new high).
	if *it.PctOff52wkH > 1e-9 {
		t.Errorf("pct_off_52wk_high %v must be <= 0", *it.PctOff52wkH)
	}
	// buy_readiness within [0,100].
	if *it.BuyReadiness < 0 || *it.BuyReadiness > 100 {
		t.Errorf("buy_readiness %v out of [0,100]", *it.BuyReadiness)
	}
	// RS rank is stamped cross-sectionally by the EOD refresh, not here.
	if it.RSRank != nil {
		t.Errorf("rs_rank should be nil at generation time, got %v", *it.RSRank)
	}
}
