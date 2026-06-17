package orbadapter

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/strategy/orb"
)

// TestNormalizeSignalWireShape proves the relocated orbadapter.NormalizeSignal
// converts a pure orb.SignalSnapshot (no json tags) to the spec-faithful
// snake_case domain.IntradayBreakoutSignal wire shape — the coverage formerly in
// publish (modularization-review.md §E3).
func TestNormalizeSignalWireShape(t *testing.T) {
	in := orb.SignalSnapshot{
		Symbol:     "MSFT",
		State:      orb.StateNoSetup,
		Strength:   0,
		UpdatedAt:  time.Date(2026, 6, 12, 14, 30, 0, 0, time.UTC),
		Generation: 3,
		StrategyID: orb.StrategyID,
		ORBHigh:    "300.10",
		ORBLow:     "298.50",
	}
	d := NormalizeSignal(in)
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
	for _, k := range []string{"orb_high", "orb_low"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("wire missing key %q", k)
		}
	}
}

// TestEvaluateSignalJSONReturnsDomainType pins that the adapter hands publish a
// domain.IntradayBreakoutSignal (§E3 bridge), so publish.NormalizeSignal's
// domain-only switch accepts it.
func TestEvaluateSignalJSONReturnsDomainType(t *testing.T) {
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
	v := s.EvaluateSignalJSON(time.Date(2024, 1, 1, 14, 30, 0, 0, time.UTC))
	if _, ok := v.(domain.IntradayBreakoutSignal); !ok {
		t.Fatalf("EvaluateSignalJSON must return domain.IntradayBreakoutSignal; got %T", v)
	}
}
