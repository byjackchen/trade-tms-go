package riskgate

// types.go ports src/portfolio/types.py (spec domain-types-money §5 [MUST-MATCH]):
// the frozen value types shared by the Allocator, RiskConstraints and the
// Gate facade. Zero engine dependency — strategies build a ProposedOrder +
// PortfolioSnapshot per check and receive a RiskDecision.
//
// All money math in this package is EXACT rational arithmetic (dec, backed by
// math/big.Rat) so it reproduces CPython's `decimal.Decimal` comparisons
// bit-for-bit. The Python pipeline performs only +, -, unary -, * and strict
// </>  comparisons on Decimals built from integer share counts, 2-dp prices and
// `Decimal(str(pct))` fractions; none of those operations round within Python's
// 28-digit context for the magnitudes used, so exact rationals give identical
// comparison results (verified by the cross-language parity tests).

import (
	"time"

	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// ProposedOrder is a strategy's intent to place an order, before risk gating
// (types.py:22-31). qty is the absolute magnitude (positive); side encodes the
// direction. price is the estimated fill price used for sizing math.
type ProposedOrder struct {
	StrategyID string            // engine strategy id (§7.7), the Allocator key
	Symbol     string            // instrument symbol
	Side       domain.SignalSide // LONG | SHORT | FLAT
	Qty        int64             // absolute magnitude (>= 0)
	Price      dec               // estimated fill price (exact)
	TS         time.Time         // bar timestamp (daily-loss windowing)
}

// RiskDecision is the outcome of running the gating pipeline (types.py:34-47).
// On rejection RuleName carries the exact reference rule name (e.g.
// "allocator.budget_exceeded") and Reason a human-readable explanation.
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

// PortfolioSnapshot is a read-only view of account state at a point in time
// (types.py:50-100). Conventions:
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

// TotalPnLToday returns realized + unrealized day P&L (types.py:72-73).
func (a PortfolioSnapshot) TotalPnLToday() dec {
	return a.RealizedPnLToday.Add(a.UnrealizedPnLToday)
}

// StrategyPosition returns the signed share count this strategy holds in symbol
// (0 if missing; types.py:75-76).
func (a PortfolioSnapshot) StrategyPosition(strategyID, symbol string) int64 {
	if a.Positions == nil {
		return 0
	}
	return a.Positions[PositionKey{StrategyID: strategyID, Symbol: symbol}]
}

// NetPositionAcrossStrategies returns the signed sum of all strategies'
// positions in symbol (types.py:78-80). Iteration order does not affect the
// result (integer addition is associative/commutative).
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
// by strategyID (types.py:82-100). Missing last_close is treated as 0 (matching
// `self.last_close.get(sym, Decimal(0))`). Iteration order is immaterial: the
// sum is over exact rationals (associative) and the final value is compared,
// not rounded.
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
