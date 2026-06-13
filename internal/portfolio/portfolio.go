package portfolio

// portfolio.go ports src/portfolio/portfolio.py (spec §5 [MUST-MATCH]): the
// gating-pipeline facade that composes Allocator → RiskConstraints. Strategy
// runners call Check before submitting; the FIRST rejection wins. This is the
// gate whose existence makes num_rejected_orders meaningful — without it the
// engine could only ever report 0 rejected orders.

// Portfolio is the pre-trade gating pipeline (portfolio.py:30-70).
type Portfolio struct {
	allocator       *Allocator
	riskConstraints *RiskConstraints
}

// NewPortfolio composes an allocator and risk constraints into the pipeline.
func NewPortfolio(allocator *Allocator, riskConstraints *RiskConstraints) *Portfolio {
	return &Portfolio{allocator: allocator, riskConstraints: riskConstraints}
}

// Allocator returns the underlying allocator.
func (p *Portfolio) Allocator() *Allocator { return p.allocator }

// RiskConstraints returns the underlying risk constraints.
func (p *Portfolio) RiskConstraints() *RiskConstraints { return p.riskConstraints }

// Check runs the gating pipeline (portfolio.py:42-61). Order: allocator
// (per-strategy budget) then risk_constraints (aggregate). First rejection
// wins; on approval returns an approving decision.
func (p *Portfolio) Check(order ProposedOrder, account AccountSnapshot) RiskDecision {
	if d := p.allocator.CheckOrderWithinBudget(order, account); !d.Approved {
		return d
	}
	if d := p.riskConstraints.Check(order, account); !d.Approved {
		return d
	}
	return Approve()
}
