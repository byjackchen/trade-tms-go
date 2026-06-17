package riskgate

// types.go (spec domain-types-money §5): the value types shared by the
// Allocator, RiskConstraints and the Gate facade. Zero engine dependency —
// strategies build a ProposedOrder + PortfolioSnapshot per check and receive a
// RiskDecision.
//
// All money math in this package is EXACT rational arithmetic (dec, backed by
// math/big.Rat) so it is bit-for-bit reproducible across platforms (arm64 vs
// x86), where rounded float arithmetic could diverge. The pipeline performs only
// +, -, unary -, * and strict </> comparisons on values built from integer share
// counts, 2-dp prices and decimal-string pct fractions; none of those operations
// round at the 28-digit decimal precision for the magnitudes used, so exact
// rationals give identical comparison results on every platform.

import (
	"time"

	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// ProposedOrder is a strategy's intent to place an order, before risk gating.
// qty is the absolute magnitude (positive); side encodes the direction. price is
// the estimated fill price used for sizing math.
type ProposedOrder struct {
	StrategyID string            // engine strategy id (§7.7), the Allocator key
	Symbol     string            // instrument symbol
	Side       domain.SignalSide // LONG | SHORT | FLAT
	Qty        int64             // absolute magnitude (>= 0)
	Price      dec               // estimated fill price (exact)
	TS         time.Time         // bar timestamp (daily-loss windowing)
}

// RiskDecision is the outcome of running the gating pipeline. On rejection
// RuleName carries the rule name (e.g. "allocator.budget_exceeded") and Reason a
// human-readable explanation.
type RiskDecision struct {
	Approved bool
	RuleName string
	Reason   string
}

// Approve returns an approving decision.
func Approve() RiskDecision { return RiskDecision{Approved: true} }

// Reject returns a rejecting decision with the given rule name and reason.
func Reject(rule, reason string) RiskDecision {
	return RiskDecision{Approved: false, RuleName: rule, Reason: reason}
}

// PositionKey identifies a (strategy_id, symbol) position slot.
type PositionKey struct {
	StrategyID string
	Symbol     string
}

// PortfolioSnapshot is a read-only view of account state at a point in time.
// Conventions:
//   - NAV = total account value (cash + market value of positions).
//   - RealizedPnLToday + UnrealizedPnLToday = total day P&L.
//   - Positions[(strategy_id, symbol)] = signed share count
//     (positive long, negative short; missing == flat).
//   - LastClose[symbol] = last close price for held-value/exposure math.
type PortfolioSnapshot struct {
	NAV                dec
	Cash               dec
	RealizedPnLToday   dec
	UnrealizedPnLToday dec
	Positions          map[PositionKey]int64
	LastClose          map[string]dec
}

// TotalPnLToday returns realized + unrealized day P&L.
func (a PortfolioSnapshot) TotalPnLToday() dec {
	return a.RealizedPnLToday.Add(a.UnrealizedPnLToday)
}

// StrategyPosition returns the signed share count this strategy holds in symbol
// (0 if missing).
func (a PortfolioSnapshot) StrategyPosition(strategyID, symbol string) int64 {
	if a.Positions == nil {
		return 0
	}
	return a.Positions[PositionKey{StrategyID: strategyID, Symbol: symbol}]
}

// NetPositionAcrossStrategies returns the signed sum of all strategies'
// positions in symbol. Iteration order does not affect the result (integer
// addition is associative/commutative).
func (a PortfolioSnapshot) NetPositionAcrossStrategies(symbol string) int64 {
	var net int64
	for k, qty := range a.Positions {
		if k.Symbol == symbol {
			net += qty
		}
	}
	return net
}

// GrossExposureForStrategy returns Σ |qty * last_close| across all symbols held
// by strategyID. Missing last_close is treated as 0. Iteration order is
// immaterial: the sum is over exact rationals (associative) and the final value
// is compared, not rounded.
func (a PortfolioSnapshot) GrossExposureForStrategy(strategyID string) dec {
	total := decZero()
	for k, qty := range a.Positions {
		if k.StrategyID != strategyID || qty == 0 {
			continue
		}
		price, ok := a.LastClose[k.Symbol]
		if !ok {
			price = decZero()
		}
		total = total.Add(decFromInt(absInt64(qty)).Mul(price))
	}
	return total
}

func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
