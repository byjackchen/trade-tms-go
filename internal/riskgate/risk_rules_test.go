package riskgate

import (
	"testing"

	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// These tests re-derive each allocator/risk limit BY HAND and exercise the
// boundary (exactly-at-limit) cases, the FLAT-bypass-during-halt case, and the
// short gross-vs-net distinction. They complement the 400-case cross-language
// parity fixture (TestRiskPipelineParity) with explicit, human-checked numbers.

func acct(nav string, realized string, positions map[PositionKey]int64, lastClose map[string]dec) PortfolioSnapshot {
	return PortfolioSnapshot{
		NAV: MustDec(nav), Cash: MustDec(nav),
		RealizedPnLToday: MustDec(realized), UnrealizedPnLToday: decZero(),
		Positions: positions, LastClose: lastClose,
	}
}

// --- Allocator: budget = nav * pct, reject iff new_gross > budget (strict). ---

func TestAllocatorBudgetBoundary(t *testing.T) {
	// SEPA 40% of 100k NAV = $40,000 budget. Hold $30k (200 @ 150), order 50 @ 200 = $10k.
	// new_gross = 30000 + 10000 = 40000. 40000 > 40000 is FALSE -> APPROVE (at limit).
	a, _ := NewAllocator([]StrategyAllocation{{"SEPA", 0.40}})
	ac := acct("100000", "0",
		map[PositionKey]int64{{"SEPA", "AAPL"}: 200},
		map[string]dec{"AAPL": MustDec("150")})
	order := ProposedOrder{StrategyID: "SEPA", Symbol: "MSFT", Side: domain.SideLong, Qty: 50, Price: MustDec("200")}
	if d := a.CheckOrderWithinBudget(order, ac); !d.Approved {
		t.Fatalf("exactly-at-budget must approve, got reject %q", d.RuleName)
	}
	// One cent over: order 50 @ 200.01 -> new_gross = 30000 + 10000.50 > 40000 -> reject.
	order.Price = MustDec("200.01")
	if d := a.CheckOrderWithinBudget(order, ac); d.Approved || d.RuleName != "allocator.budget_exceeded" {
		t.Fatalf("over-budget must reject with budget_exceeded, got %+v", d)
	}
}

func TestAllocatorUnregistered(t *testing.T) {
	a, _ := NewAllocator([]StrategyAllocation{{"SEPA", 0.40}})
	ac := acct("100000", "0", nil, nil)
	order := ProposedOrder{StrategyID: "GHOST", Symbol: "AAPL", Side: domain.SideLong, Qty: 1, Price: MustDec("1")}
	if d := a.CheckOrderWithinBudget(order, ac); d.Approved || d.RuleName != "allocator.unregistered_strategy" {
		t.Fatalf("unregistered must reject, got %+v", d)
	}
}

func TestAllocatorIndependentBooks(t *testing.T) {
	// SectorRotation's exposure must not count against SEPA's budget.
	a, _ := NewAllocator([]StrategyAllocation{{"SEPA", 0.40}, {"SectorRotation", 0.30}})
	ac := acct("100000", "0",
		map[PositionKey]int64{{"SectorRotation", "AAPL"}: 1000}, // $300k notional, irrelevant to SEPA
		map[string]dec{"AAPL": MustDec("300")})
	order := ProposedOrder{StrategyID: "SEPA", Symbol: "AAPL", Side: domain.SideLong, Qty: 100, Price: MustDec("300")}
	// SEPA budget 40k, order $30k, no SEPA exposure -> approve.
	if d := a.CheckOrderWithinBudget(order, ac); !d.Approved {
		t.Fatalf("independent books: SEPA order must approve, got %q", d.RuleName)
	}
}

func TestAllocatorShortAddsToGross(t *testing.T) {
	// A SHORT open still increases gross (order_value = qty*price, qty positive).
	a, _ := NewAllocator([]StrategyAllocation{{"Pairs", 0.20}})
	ac := acct("100000", "0", nil, nil)                                                                               // budget 20k
	order := ProposedOrder{StrategyID: "Pairs", Symbol: "KO", Side: domain.SideShort, Qty: 500, Price: MustDec("60")} // $30k
	if d := a.CheckOrderWithinBudget(order, ac); d.Approved || d.RuleName != "allocator.budget_exceeded" {
		t.Fatalf("short over budget must reject: %+v", d)
	}
}

// --- daily_loss_halt: reject iff pnl < -nav*pct (strict). ---

func TestDailyLossHaltBoundary(t *testing.T) {
	rc, _ := NewRiskConstraints(RiskConstraintsConfig{MaxSingleNamePct: 0.5, ConcentrationPct: 0.4, DailyLossHaltPct: 0.10})
	// threshold = -10% of 100k = -10000. pnl exactly -10000 -> NOT < threshold -> approve (boundary).
	at := acct("100000", "-10000", nil, nil)
	o := ProposedOrder{StrategyID: "SEPA", Symbol: "AAPL", Side: domain.SideLong, Qty: 1, Price: MustDec("1")}
	// SEPA unregistered in this rc-only test: use risk constraints only (no allocator).
	if d := rc.Check(o, at); !d.Approved {
		t.Fatalf("pnl exactly at -10%% must NOT halt, got %q", d.RuleName)
	}
	// one cent below -> halt.
	below := acct("100000", "-10000.01", nil, nil)
	if d := rc.Check(o, below); d.Approved || d.RuleName != "risk.daily_loss_halt" {
		t.Fatalf("pnl below -10%% must halt: %+v", d)
	}
}

func TestDailyLossHaltSupersedesEvenOneShare(t *testing.T) {
	rc, _ := NewRiskConstraints(DefaultRiskConstraintsConfig()) // halt 5%
	below := acct("100000", "-6000", nil, nil)
	o := ProposedOrder{StrategyID: "SEPA", Symbol: "AAPL", Side: domain.SideLong, Qty: 1, Price: MustDec("1")}
	if d := rc.Check(o, below); d.RuleName != "risk.daily_loss_halt" {
		t.Fatalf("halt must supersede even a 1-share order: %+v", d)
	}
}

func TestFlatBypassesHalt(t *testing.T) {
	rc, _ := NewRiskConstraints(DefaultRiskConstraintsConfig())
	below := acct("100000", "-50000", nil, nil) // deep halt
	flat := ProposedOrder{StrategyID: "SEPA", Symbol: "AAPL", Side: domain.SideFlat, Qty: 1000, Price: MustDec("300")}
	if d := rc.Check(flat, below); !d.Approved {
		t.Fatalf("FLAT must bypass the halt, got reject %q", d.RuleName)
	}
}

// --- max_single_name: held_value + qty*price > nav*pct (strict), GROSS. ---

func TestMaxSingleNameBoundary(t *testing.T) {
	rc, _ := NewRiskConstraints(RiskConstraintsConfig{MaxSingleNamePct: 0.20, ConcentrationPct: 0.40, DailyLossHaltPct: 0.10})
	// cap = 20% of 100k = 20000. Held 100 @ 150 = 15000; order 50 @ 150 = 7500.
	// new_value = 22500 > 20000 -> reject.
	ac := acct("100000", "0",
		map[PositionKey]int64{{"SEPA", "AAPL"}: 100},
		map[string]dec{"AAPL": MustDec("150")})
	o := ProposedOrder{StrategyID: "SEPA", Symbol: "AAPL", Side: domain.SideLong, Qty: 50, Price: MustDec("150")}
	if d := rc.Check(o, ac); d.Approved || d.RuleName != "risk.max_single_name" {
		t.Fatalf("over single-name cap must reject: %+v", d)
	}
	// At limit: held 100 @ 150 = 15000, order 33.333 not integral. Use held 0, order 133 @ 150 = 19950 (<cap), 134 @ 150 = 20100 (>cap).
	empty := acct("100000", "0", nil, nil)
	atOrUnder := ProposedOrder{StrategyID: "SEPA", Symbol: "AAPL", Side: domain.SideLong, Qty: 133, Price: MustDec("150")}
	if d := rc.Check(atOrUnder, empty); !d.Approved {
		t.Fatalf("19950 < 20000 cap must approve, got %q", d.RuleName)
	}
	// Exactly at cap: order makes new_value == cap -> approve (strict >).
	exact := ProposedOrder{StrategyID: "SEPA", Symbol: "AAPL", Side: domain.SideLong, Qty: 200, Price: MustDec("100")} // 20000
	if d := rc.Check(exact, empty); !d.Approved {
		t.Fatalf("exactly-at single-name cap must approve, got %q", d.RuleName)
	}
}

func TestMaxSingleNamePriceFallbackToOrder(t *testing.T) {
	// Held shares price falls back to order.price when last_close missing (unlike
	// the allocator's 0 fallback). Held 100, no last_close, order 1 @ 150:
	// held_value = 100*150 = 15000, new = 15000 + 150 = 15150 (cap 20k) -> approve.
	rc, _ := NewRiskConstraints(RiskConstraintsConfig{MaxSingleNamePct: 0.20, ConcentrationPct: 0.40, DailyLossHaltPct: 0.10})
	ac := acct("100000", "0",
		map[PositionKey]int64{{"SEPA", "ZZZ"}: 100}, nil) // no last_close
	o := ProposedOrder{StrategyID: "SEPA", Symbol: "ZZZ", Side: domain.SideLong, Qty: 1, Price: MustDec("150")}
	if d := rc.Check(o, ac); !d.Approved {
		t.Fatalf("price fallback case must approve, got %q", d.RuleName)
	}
	// Push order price so held re-valued high crosses cap: order 40 @ 200 ->
	// held 100*200=20000 + 40*200=8000 = 28000 > 20000 -> reject.
	o2 := ProposedOrder{StrategyID: "SEPA", Symbol: "ZZZ", Side: domain.SideLong, Qty: 40, Price: MustDec("200")}
	if d := rc.Check(o2, ac); d.RuleName != "risk.max_single_name" {
		t.Fatalf("re-valued held over cap must reject: %+v", d)
	}
}

// --- concentration: |net + signed_qty| * order.price > nav*pct (strict), NET. ---

func TestConcentrationNetCrossStrategy(t *testing.T) {
	rc, _ := NewRiskConstraints(RiskConstraintsConfig{MaxSingleNamePct: 0.50, ConcentrationPct: 0.30, DailyLossHaltPct: 0.10})
	// cap = 30% of 100k = 30000. Two strategies long AAPL: 130 held cross-strategy,
	// order +100 -> net 230 @ 150 = 34500 > 30000 -> reject.
	ac := acct("100000", "0",
		map[PositionKey]int64{{"SEPA", "AAPL"}: 100, {"SectorRotation", "AAPL"}: 30},
		map[string]dec{"AAPL": MustDec("150")})
	o := ProposedOrder{StrategyID: "SEPA", Symbol: "AAPL", Side: domain.SideLong, Qty: 100, Price: MustDec("150")}
	if d := rc.Check(o, ac); d.Approved || d.RuleName != "risk.concentration" {
		t.Fatalf("over concentration must reject: %+v", d)
	}
}

func TestConcentrationShortHedgePasses(t *testing.T) {
	// Market-neutral: 130 long held, a 100 SHORT order -> net 30 @ 150 = 4500 < cap.
	// This is the gross-vs-net distinction: gross is high, net is tiny.
	rc, _ := NewRiskConstraints(RiskConstraintsConfig{MaxSingleNamePct: 0.50, ConcentrationPct: 0.30, DailyLossHaltPct: 0.10})
	ac := acct("100000", "0",
		map[PositionKey]int64{{"SEPA", "AAPL"}: 130},
		map[string]dec{"AAPL": MustDec("150")})
	o := ProposedOrder{StrategyID: "Pairs", Symbol: "AAPL", Side: domain.SideShort, Qty: 100, Price: MustDec("150")}
	if d := rc.Check(o, ac); !d.Approved {
		t.Fatalf("short hedge to small net must pass concentration, got %q", d.RuleName)
	}
}

func TestConcentrationBoundary(t *testing.T) {
	rc, _ := NewRiskConstraints(RiskConstraintsConfig{MaxSingleNamePct: 0.50, ConcentrationPct: 0.30, DailyLossHaltPct: 0.10})
	// Exactly at cap: net 0 held, order 200 @ 150 = 30000 == cap -> approve (strict >).
	empty := acct("100000", "0", nil, nil)
	at := ProposedOrder{StrategyID: "SEPA", Symbol: "AAPL", Side: domain.SideLong, Qty: 200, Price: MustDec("150")}
	if d := rc.Check(at, empty); !d.Approved {
		t.Fatalf("exactly-at concentration cap must approve, got %q", d.RuleName)
	}
	// One share over -> reject.
	over := ProposedOrder{StrategyID: "SEPA", Symbol: "AAPL", Side: domain.SideLong, Qty: 201, Price: MustDec("150")}
	if d := rc.Check(over, empty); d.RuleName != "risk.concentration" {
		t.Fatalf("over concentration cap must reject: %+v", d)
	}
}

// --- pipeline rule order: first rejection wins. ---

func TestPipelineFirstRejectionWins(t *testing.T) {
	a, _ := NewAllocator([]StrategyAllocation{{"SEPA", 0.40}})
	rc, _ := NewRiskConstraints(RiskConstraintsConfig{MaxSingleNamePct: 0.20, ConcentrationPct: 0.40, DailyLossHaltPct: 0.10})
	pf := NewGate(a, rc)
	// Order over BOTH budget (40k) and single-name (20k): allocator runs first ->
	// reports allocator.budget_exceeded.
	empty := acct("100000", "0", nil, nil)
	o := ProposedOrder{StrategyID: "SEPA", Symbol: "AAPL", Side: domain.SideLong, Qty: 1000, Price: MustDec("100")} // $100k
	if d := pf.Check(o, empty); d.RuleName != "allocator.budget_exceeded" {
		t.Fatalf("allocator must win over risk: %+v", d)
	}
	// Within budget but over single-name: 30k order (< 40k budget) but > 20k single-name.
	o2 := ProposedOrder{StrategyID: "SEPA", Symbol: "AAPL", Side: domain.SideLong, Qty: 300, Price: MustDec("100")} // $30k
	if d := pf.Check(o2, empty); d.RuleName != "risk.max_single_name" {
		t.Fatalf("within budget but over single-name must report max_single_name: %+v", d)
	}
}
