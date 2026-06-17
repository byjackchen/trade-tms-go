package riskgate

// bridge.go converts the engine/accounting domain value types (fixed-point
// domain.Money / domain.Price / domain.Qty, keyed by domain.StrategySymbol)
// into the exact-rational portfolio gating value types (ProposedOrder /
// PortfolioSnapshot, money as `dec`). It is the single seam between the
// execution engine and the pre-trade risk pipeline: the engine never touches
// the unexported `dec` type, and the portfolio package never imports the
// engine — the conversion lives HERE and flows one way (engine -> portfolio).
//
// Mirrors src/runner/portfolio_glue.py:build_snapshot_from_nautilus +
// _base/runner.py:_gate: the ProposedOrder price is the bar's last close, the
// snapshot NAV/Cash are equity (balance_total), today-P&L is 0 in backtest
// (daily-loss-halt dormant), positions are signed shares keyed by
// (strategy_id, symbol), and last_close is the per-symbol mark.

import (
	"time"

	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// NewProposedOrder builds a ProposedOrder from engine value types. side is the
// strategy-level SignalSide (LONG/SHORT/FLAT); qty is the absolute magnitude;
// price is the estimated fill price (the bar's last close, per the reference
// _gate which reads self._last_close.get(symbol, Decimal(0))).
func NewProposedOrder(strategyID, symbol string, side domain.SignalSide, qty domain.Qty, price domain.Price, ts time.Time) ProposedOrder {
	return ProposedOrder{
		StrategyID: strategyID,
		Symbol:     symbol,
		Side:       side,
		Qty:        int64(qty),
		Price:      DecFromPrice(price),
		TS:         ts,
	}
}

// SnapshotFromDomain converts a domain.PortfolioSnapshot (the accounting layer's
// view) into the portfolio gating PortfolioSnapshot. NAV and Cash both come from
// the snapshot's NAV/Cash (the reference sets cash == NAV); today-P&L fields
// map straight across (0 in backtest). Positions and last-close are converted
// element-wise. Iteration order is immaterial — the maps are rebuilt and the
// downstream sums are over exact rationals / integers.
func SnapshotFromDomain(s domain.PortfolioSnapshot) PortfolioSnapshot {
	positions := make(map[PositionKey]int64, len(s.Positions))
	for k, q := range s.Positions {
		positions[PositionKey{StrategyID: k.StrategyID, Symbol: k.Symbol}] = int64(q)
	}
	lastClose := make(map[string]dec, len(s.LastClose))
	for sym, px := range s.LastClose {
		lastClose[sym] = DecFromPrice(px)
	}
	return PortfolioSnapshot{
		NAV:                DecFromMoney(s.NAV),
		Cash:               DecFromMoney(s.Cash),
		RealizedPnLToday:   DecFromMoney(s.RealizedPnLToday),
		UnrealizedPnLToday: DecFromMoney(s.UnrealizedPnLToday),
		Positions:          positions,
		LastClose:          lastClose,
	}
}
