package riskgate

// risk_constraints.go ports src/portfolio/risk_constraints.py (spec §5
// [MUST-MATCH]): three aggregate hard rules evaluated per ProposedOrder, each
// short-circuiting (first rejection wins) in the fixed order
// daily_loss_halt → max_single_name → concentration. FLAT and qty<=0 always
// approve (closes reduce risk, including during a daily-loss halt). All money
// comparisons are exact.

import (
	"fmt"

	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// RiskConstraintsConfig holds the three rule thresholds (risk_constraints.py:
// 31-44). Defaults: max_single_name 0.20, concentration 0.30, daily_loss_halt
// 0.05. Each must be strictly in (0, 1].
type RiskConstraintsConfig struct {
	MaxSingleNamePct float64
	ConcentrationPct float64
	DailyLossHaltPct float64
}

// DefaultRiskConstraintsConfig returns the reference defaults.
func DefaultRiskConstraintsConfig() RiskConstraintsConfig {
	return RiskConstraintsConfig{
		MaxSingleNamePct: 0.20,
		ConcentrationPct: 0.30,
		DailyLossHaltPct: 0.05,
	}
}

// Validate checks each pct is strictly in (0, 1] (risk_constraints.py:38-44).
func (c RiskConstraintsConfig) Validate() error {
	for name, v := range map[string]float64{
		"max_single_name_pct": c.MaxSingleNamePct,
		"concentration_pct":   c.ConcentrationPct,
		"daily_loss_halt_pct": c.DailyLossHaltPct,
	} {
		if !(v > 0 && v <= 1) {
			return fmt.Errorf("%s must be in (0, 1], got %v", name, v)
		}
	}
	return nil
}

// RiskConstraints applies the aggregate hard rules (risk_constraints.py:47-141).
type RiskConstraints struct {
	cfg RiskConstraintsConfig
}

// NewRiskConstraints builds a RiskConstraints from cfg (validated).
func NewRiskConstraints(cfg RiskConstraintsConfig) (*RiskConstraints, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &RiskConstraints{cfg: cfg}, nil
}

// Config returns the active configuration.
func (r *RiskConstraints) Config() RiskConstraintsConfig { return r.cfg }

// Check runs the three rules in order; first rejection wins
// (risk_constraints.py:60-83). FLAT or qty<=0 → approve.
func (r *RiskConstraints) Check(order ProposedOrder, account PortfolioSnapshot) RiskDecision {
	if order.Side == domain.SideFlat || order.Qty <= 0 {
		return Approve()
	}
	if d := r.checkDailyLossHalt(account); !d.Approved {
		return d
	}
	if d := r.checkMaxSingleName(order, account); !d.Approved {
		return d
	}
	if d := r.checkConcentration(order, account); !d.Approved {
		return d
	}
	return Approve()
}

// checkDailyLossHalt: pnl < -nav*pct (strict) → reject (risk_constraints.py:89-101).
func (r *RiskConstraints) checkDailyLossHalt(account PortfolioSnapshot) RiskDecision {
	threshold := account.NAV.Mul(decFromPctFloat(r.cfg.DailyLossHaltPct)).Neg()
	pnl := account.TotalPnLToday()
	if pnl.Cmp(threshold) < 0 {
		return Reject("risk.daily_loss_halt",
			fmt.Sprintf("day P&L $%s is below halt threshold $%s (%.1f%% NAV)",
				pnl, threshold, r.cfg.DailyLossHaltPct*100))
	}
	return Approve()
}

// checkMaxSingleName: held_value + qty*price > nav*pct (strict) → reject
// (risk_constraints.py:103-124). held_value uses last_close.get(symbol, price).
func (r *RiskConstraints) checkMaxSingleName(order ProposedOrder, account PortfolioSnapshot) RiskDecision {
	heldQty := absInt64(account.StrategyPosition(order.StrategyID, order.Symbol))
	lastClose, ok := account.LastClose[order.Symbol]
	if !ok {
		lastClose = order.Price
	}
	heldValue := decFromInt(heldQty).Mul(lastClose)
	newValue := heldValue.Add(decFromInt(order.Qty).Mul(order.Price))

	cap := account.NAV.Mul(decFromPctFloat(r.cfg.MaxSingleNamePct))
	if newValue.Cmp(cap) > 0 {
		return Reject("risk.max_single_name",
			fmt.Sprintf("%s %s gross $%s would exceed single-name cap $%s (%.1f%% NAV)",
				order.StrategyID, order.Symbol, newValue, cap, r.cfg.MaxSingleNamePct*100))
	}
	return Approve()
}

// checkConcentration: |net_across_strategies + signed_qty| * order.price >
// nav*pct (strict) → reject (risk_constraints.py:126-141). signed_qty = +qty if
// LONG else -qty. Uses order.price for the whole net (per spec note).
func (r *RiskConstraints) checkConcentration(order ProposedOrder, account PortfolioSnapshot) RiskDecision {
	currentNet := account.NetPositionAcrossStrategies(order.Symbol)
	signedQty := order.Qty
	if order.Side != domain.SideLong {
		signedQty = -order.Qty
	}
	newNet := currentNet + signedQty

	newNetValue := decFromInt(absInt64(newNet)).Mul(order.Price)
	cap := account.NAV.Mul(decFromPctFloat(r.cfg.ConcentrationPct))
	if newNetValue.Cmp(cap) > 0 {
		return Reject("risk.concentration",
			fmt.Sprintf("net %s across all strategies = %d shares ($%s) would exceed concentration cap $%s (%.1f%% NAV)",
				order.Symbol, newNet, newNetValue, cap, r.cfg.ConcentrationPct*100))
	}
	return Approve()
}
