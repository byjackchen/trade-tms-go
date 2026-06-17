package domain

// account.go defines PortfolioSnapshot, the read-only runtime account-book view
// consumed by the risk pipeline (spec §2.9).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

// StrategySymbol is the (strategy_id, symbol) composite key of the positions
// book. StrategyID is the ENGINE strategy id (§7.7).
type StrategySymbol struct {
	StrategyID string `json:"strategy_id"`
	Symbol     string `json:"symbol"`
}

// PortfolioSnapshot is a point-in-time, read-only view of account state.
// Treat as immutable: never mutate the maps of a snapshot you received
// (NewPortfolioSnapshot and Clone deep-copy them, including last_close).
//
// Conventions:
//   - NAV = total account value. Cash is set equal to NAV ("balance_total
//     already accounts for margin"); a future engine may track true cash
//     separately, but the value fed to the risk pipeline must remain NAV so
//     sizing and gating see the same equity.
//   - RealizedPnLToday/UnrealizedPnLToday default to 0 in backtest, which
//     keeps the daily-loss-halt rule dormant.
//   - Positions[(strategy_id, symbol)] = signed shares; 0/missing = flat.
type PortfolioSnapshot struct {
	NAV                Money
	Cash               Money
	RealizedPnLToday   Money
	UnrealizedPnLToday Money
	Positions          map[StrategySymbol]Qty
	LastClose          map[string]Price
}

// NewPortfolioSnapshot builds a snapshot, deep-copying both maps (nil maps are
// allowed and become empty maps).
func NewPortfolioSnapshot(
	nav, cash, realizedToday, unrealizedToday Money,
	positions map[StrategySymbol]Qty,
	lastClose map[string]Price,
) PortfolioSnapshot {
	return PortfolioSnapshot{
		NAV:                nav,
		Cash:               cash,
		RealizedPnLToday:   realizedToday,
		UnrealizedPnLToday: unrealizedToday,
		Positions:          clonePositions(positions),
		LastClose:          cloneLastClose(lastClose),
	}
}

// Clone returns a deep copy.
func (a PortfolioSnapshot) Clone() PortfolioSnapshot {
	return PortfolioSnapshot{
		NAV:                a.NAV,
		Cash:               a.Cash,
		RealizedPnLToday:   a.RealizedPnLToday,
		UnrealizedPnLToday: a.UnrealizedPnLToday,
		Positions:          clonePositions(a.Positions),
		LastClose:          cloneLastClose(a.LastClose),
	}
}

// TotalPnLToday returns realized + unrealized day P&L.
func (a PortfolioSnapshot) TotalPnLToday() (Money, error) {
	v, err := a.RealizedPnLToday.Add(a.UnrealizedPnLToday)
	if err != nil {
		return 0, fmt.Errorf("total_pnl_today: %w", err)
	}
	return v, nil
}

// StrategyPosition returns the signed share count for (strategyID, symbol),
// 0 when absent.
func (a PortfolioSnapshot) StrategyPosition(strategyID, symbol string) Qty {
	return a.Positions[StrategySymbol{StrategyID: strategyID, Symbol: symbol}]
}

// NetPositionAcrossStrategies sums all strategies' signed positions in the
// symbol.
func (a PortfolioSnapshot) NetPositionAcrossStrategies(symbol string) (Qty, error) {
	var net Qty
	for key, qty := range a.Positions {
		if key.Symbol != symbol {
			continue
		}
		n, err := net.Add(qty)
		if err != nil {
			return 0, fmt.Errorf("net position for %s: %w", symbol, err)
		}
		net = n
	}
	return net, nil
}

// GrossExposureForStrategy returns Σ |qty| × last_close over the strategy's
// non-zero positions. Symbols missing from LastClose contribute 0 — positions
// with unknown price are invisible to the budget.
func (a PortfolioSnapshot) GrossExposureForStrategy(strategyID string) (Money, error) {
	var total Money
	for key, qty := range a.Positions {
		if key.StrategyID != strategyID || qty == 0 {
			continue
		}
		price, ok := a.LastClose[key.Symbol]
		if !ok || price == 0 {
			continue // contributes 0
		}
		absQty, err := qty.Abs()
		if err != nil {
			return 0, fmt.Errorf("gross exposure for %s/%s: %w", strategyID, key.Symbol, err)
		}
		// The contribution is abs(qty) * price; it keeps the price's sign
		// (only relevant for a nonsensical negative close).
		value, err := price.MulQty(absQty)
		if err != nil {
			return 0, fmt.Errorf("gross exposure for %s/%s: %w", strategyID, key.Symbol, err)
		}
		total, err = total.Add(value)
		if err != nil {
			return 0, fmt.Errorf("gross exposure for %s: %w", strategyID, err)
		}
	}
	return total, nil
}

// ---------------------------------------------------------------------------
// JSON
// ---------------------------------------------------------------------------

// positionEntryJSON flattens the composite-keyed positions map for JSON (Go
// maps with struct keys cannot marshal natively, so this defines the canonical
// encoding).
type positionEntryJSON struct {
	StrategyID string `json:"strategy_id"`
	Symbol     string `json:"symbol"`
	Qty        Qty    `json:"qty"`
}

type portfolioSnapshotJSON struct {
	NAV                Money               `json:"nav"`
	Cash               Money               `json:"cash"`
	RealizedPnLToday   Money               `json:"realized_pnl_today"`
	UnrealizedPnLToday Money               `json:"unrealized_pnl_today"`
	Positions          []positionEntryJSON `json:"positions"`
	LastClose          map[string]Price    `json:"last_close"`
}

// MarshalJSON encodes the snapshot with positions as a deterministic array
// sorted by (strategy_id, symbol).
func (a PortfolioSnapshot) MarshalJSON() ([]byte, error) {
	entries := make([]positionEntryJSON, 0, len(a.Positions))
	for key, qty := range a.Positions {
		entries = append(entries, positionEntryJSON{StrategyID: key.StrategyID, Symbol: key.Symbol, Qty: qty})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].StrategyID != entries[j].StrategyID {
			return entries[i].StrategyID < entries[j].StrategyID
		}
		return entries[i].Symbol < entries[j].Symbol
	})
	lastClose := a.LastClose
	if lastClose == nil {
		lastClose = map[string]Price{}
	}
	return json.Marshal(portfolioSnapshotJSON{
		NAV:                a.NAV,
		Cash:               a.Cash,
		RealizedPnLToday:   a.RealizedPnLToday,
		UnrealizedPnLToday: a.UnrealizedPnLToday,
		Positions:          entries,
		LastClose:          lastClose,
	})
}

// UnmarshalJSON decodes the canonical encoding produced by MarshalJSON.
// Duplicate (strategy_id, symbol) entries are rejected.
func (a *PortfolioSnapshot) UnmarshalJSON(b []byte) error {
	if string(bytes.TrimSpace(b)) == "null" {
		return nil
	}
	var raw portfolioSnapshotJSON
	if err := json.Unmarshal(b, &raw); err != nil {
		return fmt.Errorf("decoding account snapshot: %w", err)
	}
	positions := make(map[StrategySymbol]Qty, len(raw.Positions))
	for _, e := range raw.Positions {
		key := StrategySymbol{StrategyID: e.StrategyID, Symbol: e.Symbol}
		if _, dup := positions[key]; dup {
			return fmt.Errorf("%w: duplicate position entry %s/%s", ErrInvalidArgument, e.StrategyID, e.Symbol)
		}
		positions[key] = e.Qty
	}
	lastClose := raw.LastClose
	if lastClose == nil {
		lastClose = map[string]Price{}
	}
	*a = PortfolioSnapshot{
		NAV:                raw.NAV,
		Cash:               raw.Cash,
		RealizedPnLToday:   raw.RealizedPnLToday,
		UnrealizedPnLToday: raw.UnrealizedPnLToday,
		Positions:          positions,
		LastClose:          lastClose,
	}
	return nil
}

func clonePositions(in map[StrategySymbol]Qty) map[StrategySymbol]Qty {
	out := make(map[StrategySymbol]Qty, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneLastClose(in map[string]Price) map[string]Price {
	out := make(map[string]Price, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
