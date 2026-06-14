package orbadapter

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/strategy/orb"
)

// TestNormalizeIntentWireShape proves the relocated orbadapter.NormalizeIntent
// converts a pure orb.SignalIntent (no json tags) to the spec-faithful
// snake_case domain.IntradayBreakoutIntent wire shape — the coverage formerly in
// publish (modularization-review.md §E3).
func TestNormalizeIntentWireShape(t *testing.T) {
	in := orb.SignalIntent{
		Symbol:     "MSFT",
		State:      orb.StateNoSetup,
		Strength:   0,
		UpdatedAt:  time.Date(2026, 6, 12, 14, 30, 0, 0, time.UTC),
		Generation: 3,
		StrategyID: orb.StrategyID,
		ORBHigh:    "300.10",
		ORBLow:     "298.50",
	}
	d := NormalizeIntent(in)
	if d.Symbol != "MSFT" || d.State != domain.StateNoSetup || d.Generation != 3 {
		t.Fatalf("discriminators wrong: %+v", d)
	}
	body, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	if m["strategy_id"] != "intraday_breakout" || m["state"] != "no_setup" {
		t.Fatalf("wire discriminators wrong: %v", m)
	}
	for _, k := range []string{"orb_high", "orb_low", "atr_at_open"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("wire missing key %q", k)
		}
	}
}

// TestEvaluateIntentJSONReturnsDomainType pins that the adapter hands publish a
// domain.IntradayBreakoutIntent (§E3 bridge), so publish.NormalizeIntent's
// domain-only switch accepts it.
func TestEvaluateIntentJSONReturnsDomainType(t *testing.T) {
	gen, err := orb.New(orb.Config{
		Symbol: "MSFT", EquityProvider: func() float64 { return 100000 },
		RiskPct: 1.0, RangeMinutes: 30, VolMultiple: 1.5, ProfitTargetR: 2.0,
		HardStopPct: 1.0, EODExitTime: "15:55", Timezone: "America/New_York",
	})
	if err != nil {
		t.Fatalf("orb.New: %v", err)
	}
	s, err := New("IntradayBreakoutRunner-000", gen)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	v := s.EvaluateIntentJSON(time.Date(2024, 1, 1, 14, 30, 0, 0, time.UTC))
	if _, ok := v.(domain.IntradayBreakoutIntent); !ok {
		t.Fatalf("EvaluateIntentJSON must return domain.IntradayBreakoutIntent; got %T", v)
	}
}
