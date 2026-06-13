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

// PortfolioHealthSnapshot is a read-only aggregate of portfolio risk state at a
// point in time (portfolio.py:22-30, spec §4.2). All money fields are exact
// dec; ratios use 28-significant-digit division (dec.Quo) mirroring CPython's
// decimal default context.
type PortfolioHealthSnapshot struct {
	DayPnL           dec  // realized + unrealized day P&L
	DayPnLPct        dec  // day_pnl / nav (0 if nav <= 0)
	DailyLossHalt    bool // day_pnl < -nav*halt_pct (strict, mirrors §3.3)
	HaltHeadroomPct  dec  // (day_pnl - threshold)/nav, clamped to 0 when halted
	ConcentrationPct dec  // largest |net_qty * last_close| / nav across symbols
}

// HealthSnapshot computes a PortfolioHealthSnapshot from an AccountSnapshot
// (portfolio.py:63-104, spec §4.3). Pure read — mutates nothing. The
// daily-loss-halt threshold logic mirrors RiskConstraints exactly (strict <).
func (p *Portfolio) HealthSnapshot(account AccountSnapshot) PortfolioHealthSnapshot {
	nav := account.NAV
	navPositive := nav.Sign() > 0

	dayPnL := account.TotalPnLToday()
	dayPnLPct := decZero()
	if navPositive {
		dayPnLPct = dayPnL.Quo(nav)
	}

	haltPct := decFromPctFloat(p.riskConstraints.Config().DailyLossHaltPct)
	threshold := nav.Mul(haltPct).Neg()
	halted := dayPnL.Cmp(threshold) < 0

	headroom := decZero()
	if !halted && navPositive {
		headroom = dayPnL.Sub(threshold).Quo(nav)
	}

	// Concentration: largest |net_qty * last_close| / NAV across distinct
	// symbols; NET (not gross); skip net-0 symbols; missing price -> 0.
	concentration := decZero()
	if navPositive {
		seen := make(map[string]struct{}, len(account.Positions))
		for k := range account.Positions {
			if _, done := seen[k.Symbol]; done {
				continue
			}
			seen[k.Symbol] = struct{}{}
			net := account.NetPositionAcrossStrategies(k.Symbol)
			if net == 0 {
				continue
			}
			px, ok := account.LastClose[k.Symbol]
			if !ok {
				px = decZero()
			}
			value := decFromInt(absInt64(net)).Mul(px)
			pct := value.Quo(nav)
			if pct.Cmp(concentration) > 0 {
				concentration = pct
			}
		}
	}

	return PortfolioHealthSnapshot{
		DayPnL:           dayPnL,
		DayPnLPct:        dayPnLPct,
		DailyLossHalt:    halted,
		HaltHeadroomPct:  headroom,
		ConcentrationPct: concentration,
	}
}
