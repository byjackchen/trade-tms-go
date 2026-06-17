package riskgate

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/byjackchen/trade-tms-go/internal/domain"
)

func sideFromName(s string) domain.SignalSide {
	switch s {
	case "LONG":
		return domain.SideLong
	case "SHORT":
		return domain.SideShort
	default:
		return domain.SideFlat
	}
}

// TestRiskPipelineParity replays 400 cases captured from the Python reference
// (riskgate.Allocator/RiskConstraints/Gate) and asserts the Go pipeline
// reaches the SAME approve/reject decision and rule name for the allocator
// stage, the risk-constraints stage and the composed pipeline — including all
// four rule names (allocator.unregistered_strategy, allocator.budget_exceeded,
// risk.daily_loss_halt, risk.max_single_name, risk.concentration). This is the
// gate that makes num_rejected_orders meaningful.
func TestRiskPipelineParity(t *testing.T) {
	raw, err := os.ReadFile("testdata/risk_parity.json")
	if err != nil {
		t.Skipf("parity fixture missing (%v); generated from the Python reference", err)
	}
	type posJSON struct {
		Strat string `json:"strat"`
		Sym   string `json:"sym"`
		Qty   int64  `json:"qty"`
	}
	type decisionJSON struct {
		Approved bool   `json:"approved"`
		Rule     string `json:"rule"`
	}
	var cases []struct {
		NAV       string            `json:"nav"`
		Realized  string            `json:"realized"`
		Positions []posJSON         `json:"positions"`
		LastClose map[string]string `json:"last_close"`
		Order     struct {
			Strat string `json:"strat"`
			Sym   string `json:"sym"`
			Side  string `json:"side"`
			Qty   int64  `json:"qty"`
			Price string `json:"price"`
		} `json:"order"`
		Pipeline decisionJSON `json:"pipeline"`
		Alloc    decisionJSON `json:"alloc"`
		Risk     decisionJSON `json:"risk"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}

	alloc, err := NewAllocator([]StrategyAllocation{
		{"SEPARunner-000", 0.40}, {"SectorRotationRunner-000", 0.30}, {"PairsRunner-000", 0.20},
	})
	if err != nil {
		t.Fatal(err)
	}
	rc, err := NewRiskConstraints(RiskConstraintsConfig{MaxSingleNamePct: 0.50, ConcentrationPct: 0.40, DailyLossHaltPct: 0.10})
	if err != nil {
		t.Fatal(err)
	}
	pf := NewGate(alloc, rc)

	chk := func(t *testing.T, stage string, got RiskDecision, want struct {
		Approved bool   `json:"approved"`
		Rule     string `json:"rule"`
	}) {
		if got.Approved != want.Approved || got.RuleName != want.Rule {
			t.Fatalf("%s: got (approved=%v rule=%q) want (approved=%v rule=%q)",
				stage, got.Approved, got.RuleName, want.Approved, want.Rule)
		}
	}

	for i, c := range cases {
		positions := make(map[PositionKey]int64, len(c.Positions))
		for _, p := range c.Positions {
			positions[PositionKey{StrategyID: p.Strat, Symbol: p.Sym}] = p.Qty
		}
		lastClose := make(map[string]dec, len(c.LastClose))
		for k, v := range c.LastClose {
			lastClose[k] = MustDec(v)
		}
		acct := PortfolioSnapshot{
			NAV:                MustDec(c.NAV),
			Cash:               MustDec(c.NAV),
			RealizedPnLToday:   MustDec(c.Realized),
			UnrealizedPnLToday: decZero(),
			Positions:          positions,
			LastClose:          lastClose,
		}
		order := ProposedOrder{
			StrategyID: c.Order.Strat,
			Symbol:     c.Order.Sym,
			Side:       sideFromName(c.Order.Side),
			Qty:        c.Order.Qty,
			Price:      MustDec(c.Order.Price),
		}
		chk(t, "alloc", alloc.CheckOrderWithinBudget(order, acct), c.Alloc)
		chk(t, "risk", rc.Check(order, acct), c.Risk)
		chk(t, "pipeline", pf.Check(order, acct), c.Pipeline)
		_ = i
	}
	t.Logf("%d risk-pipeline cases parity-verified", len(cases))
}

func TestAllocatorValidation(t *testing.T) {
	if _, err := NewAllocator(nil); err == nil {
		t.Fatal("empty allocations must error")
	}
	if _, err := NewAllocator([]StrategyAllocation{{"a", 0.5}, {"a", 0.3}}); err == nil {
		t.Fatal("duplicate strategy_id must error")
	}
	if _, err := NewAllocator([]StrategyAllocation{{"a", 0}}); err == nil {
		t.Fatal("capital_pct 0 must error")
	}
	if _, err := NewAllocator([]StrategyAllocation{{"a", 1.5}}); err == nil {
		t.Fatal("capital_pct > 1 must error")
	}
	if _, err := NewAllocator([]StrategyAllocation{{"a", 0.6}, {"b", 0.6}}); err == nil {
		t.Fatal("sum > 1 must error")
	}
	// Sum within 1e-9 slack is allowed.
	if _, err := NewAllocator([]StrategyAllocation{{"a", 0.5}, {"b", 0.5}}); err != nil {
		t.Fatalf("sum == 1.0 must be allowed: %v", err)
	}
}

func TestRiskConstraintsValidation(t *testing.T) {
	for _, cfg := range []RiskConstraintsConfig{
		{MaxSingleNamePct: 0, ConcentrationPct: 0.3, DailyLossHaltPct: 0.05},
		{MaxSingleNamePct: 0.2, ConcentrationPct: 1.5, DailyLossHaltPct: 0.05},
		{MaxSingleNamePct: 0.2, ConcentrationPct: 0.3, DailyLossHaltPct: -0.1},
	} {
		if _, err := NewRiskConstraints(cfg); err == nil {
			t.Fatalf("invalid config %+v must error", cfg)
		}
	}
}

// TestClosesAlwaysApprove asserts FLAT and qty<=0 always pass every stage even
// during a daily-loss halt (closes reduce risk).
func TestClosesAlwaysApprove(t *testing.T) {
	alloc, _ := NewAllocator([]StrategyAllocation{{"S", 0.40}})
	rc, _ := NewRiskConstraints(DefaultRiskConstraintsConfig())
	pf := NewGate(alloc, rc)
	acct := PortfolioSnapshot{
		NAV:              MustDec("100000"),
		RealizedPnLToday: MustDec("-50000"), // way past the halt threshold
		Positions:        map[PositionKey]int64{{"S", "AAPL"}: 1000},
		LastClose:        map[string]dec{"AAPL": MustDec("300")},
	}
	flat := ProposedOrder{StrategyID: "S", Symbol: "AAPL", Side: domain.SideFlat, Qty: 1000, Price: MustDec("300")}
	if d := pf.Check(flat, acct); !d.Approved {
		t.Fatalf("FLAT must approve during halt, got reject %q", d.RuleName)
	}
	zero := ProposedOrder{StrategyID: "S", Symbol: "AAPL", Side: domain.SideLong, Qty: 0, Price: MustDec("300")}
	if d := pf.Check(zero, acct); !d.Approved {
		t.Fatalf("qty<=0 must approve, got reject %q", d.RuleName)
	}
}
