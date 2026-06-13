package portfolio

// allocator.go ports src/portfolio/allocator.py (spec §5 [MUST-MATCH]):
// per-strategy capital-budget enforcement. Each strategy gets a fixed fraction
// of NAV; a proposed order is rejected if it would push that strategy's GROSS
// dollar exposure beyond its budget.

import (
	"fmt"

	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// StrategyAllocation is one row of the allocation table (allocator.py:18-22):
// a strategy id and its capital fraction in (0, 1].
type StrategyAllocation struct {
	StrategyID string
	CapitalPct float64
}

// Allocator enforces per-strategy capital budgets (allocator.py:25-97).
type Allocator struct {
	table map[string]float64
}

// NewAllocator validates and builds an Allocator (allocator.py:35-53):
//   - at least one allocation;
//   - no duplicate strategy_id;
//   - each capital_pct strictly in (0, 1];
//   - Σ capital_pct <= 1.0 + 1e-9.
//
// Errors mirror the reference ValueError messages.
func NewAllocator(allocations []StrategyAllocation) (*Allocator, error) {
	if len(allocations) == 0 {
		return nil, fmt.Errorf("Allocator requires at least one StrategyAllocation")
	}
	table := make(map[string]float64, len(allocations))
	total := 0.0
	for _, a := range allocations {
		if _, dup := table[a.StrategyID]; dup {
			return nil, fmt.Errorf("duplicate strategy_id: %s", a.StrategyID)
		}
		if !(a.CapitalPct > 0 && a.CapitalPct <= 1.0) {
			return nil, fmt.Errorf("capital_pct for %s must be in (0, 1], got %v", a.StrategyID, a.CapitalPct)
		}
		table[a.StrategyID] = a.CapitalPct
		total += a.CapitalPct
	}
	if total > 1.0+1e-9 {
		return nil, fmt.Errorf("allocations sum to %.4f > 1.0", total)
	}
	return &Allocator{table: table}, nil
}

// BudgetPct returns the fraction of NAV allocated to strategyID, or 0 if it is
// not registered (allocator.py:55-57).
func (a *Allocator) BudgetPct(strategyID string) float64 {
	return a.table[strategyID]
}

// budgetDollars converts the budget % to absolute $ given current NAV
// (allocator.py:59-62): nav * Decimal(str(pct)).
func (a *Allocator) budgetDollars(strategyID string, account AccountSnapshot) dec {
	return account.NAV.Mul(decFromPctFloat(a.BudgetPct(strategyID)))
}

// CheckOrderWithinBudget rejects an order that would push the strategy's gross
// exposure over budget (allocator.py:64-97). FLAT and qty<=0 always approve.
// Rule names: "allocator.unregistered_strategy" (no/zero budget),
// "allocator.budget_exceeded" (strict >). The budget comparison is exact.
func (a *Allocator) CheckOrderWithinBudget(order ProposedOrder, account AccountSnapshot) RiskDecision {
	if order.Side == domain.SideFlat || order.Qty <= 0 {
		return Approve()
	}

	budget := a.budgetDollars(order.StrategyID, account)
	if budget.Sign() <= 0 {
		return Reject("allocator.unregistered_strategy",
			fmt.Sprintf("strategy_id '%s' has no allocation", order.StrategyID))
	}

	currentGross := account.GrossExposureForStrategy(order.StrategyID)
	orderValue := decFromInt(order.Qty).Mul(order.Price)
	newGross := currentGross.Add(orderValue)

	if newGross.Cmp(budget) > 0 {
		return Reject("allocator.budget_exceeded",
			fmt.Sprintf("%s gross exposure $%s would exceed budget $%s (current $%s, order $%s)",
				order.StrategyID, newGross, budget, currentGross, orderValue))
	}
	return Approve()
}
