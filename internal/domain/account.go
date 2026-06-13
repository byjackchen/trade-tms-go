package domain

// account.go defines AccountSnapshot, the read-only account view consumed by
// the risk pipeline, mirroring the frozen Python dataclass
// src/portfolio/types.py:50-94 (spec §2.9 [MUST-MATCH]).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

// StrategySymbol is the (strategy_id, symbol) composite key of the positions
// book — the Python tuple key. StrategyID is the ENGINE strategy id (§7.7).
type StrategySymbol struct {
	StrategyID string `json:"strategy_id"`
	Symbol     string `json:"symbol"`
}

// AccountSnapshot is a point-in-time, read-only view of account state.
// Treat as immutable: never mutate the maps of a snapshot you received
// (NewAccountSnapshot and Clone deep-copy them; the Python glue likewise
// copies last_close).
//
// Conventions (Python docstring [MUST-MATCH]):
//   - NAV = total account value. The reference glue sets Cash = NAV
//     ("balance_total already accounts for margin"); a Go engine may track
//     true cash separately [IMPROVE], but the value fed to the risk pipeline
//     must remain NAV for parity.
//   - RealizedPnLToday/UnrealizedPnLToday default to 0 in backtest, which
//     keeps the daily-loss-halt rule dormant — also parity-relevant.
//   - Positions[(strategy_id, symbol)] = signed shares; 0/missing = flat.
type AccountSnapshot struct {
	NAV                Money
	Cash               Money
	RealizedPnLToday   Money
	UnrealizedPnLToday Money
	Positions          map[StrategySymbol]Qty
	LastClose          map[string]Price
}

// NewAccountSnapshot builds a snapshot, deep-copying both maps (nil maps are
// allowed and become empty maps, like the Python default_factory=dict).
func NewAccountSnapshot(
	nav, cash, realizedToday, unrealizedToday Money,
	positions map[StrategySymbol]Qty,
	lastClose map[string]Price,
) AccountSnapshot {
	return AccountSnapshot{
		NAV:                nav,
		Cash:               cash,
		RealizedPnLToday:   realizedToday,
		UnrealizedPnLToday: unrealizedToday,
		Positions:          clonePositions(positions),
		LastClose:          cloneLastClose(lastClose),
	}
}

// Clone returns a deep copy.
func (a AccountSnapshot) Clone() AccountSnapshot {
	return AccountSnapshot{
		NAV:                a.NAV,
		Cash:               a.Cash,
		RealizedPnLToday:   a.RealizedPnLToday,
		UnrealizedPnLToday: a.UnrealizedPnLToday,
		Positions:          clonePositions(a.Positions),
		LastClose:          cloneLastClose(a.LastClose),
	}
}

// TotalPnLToday returns realized + unrealized day P&L
// (types.py:71-72 [MUST-MATCH]).
func (a AccountSnapshot) TotalPnLToday() (Money, error) {
	v, err := a.RealizedPnLToday.Add(a.UnrealizedPnLToday)
	if err != nil {
		return 0, fmt.Errorf("total_pnl_today: %w", err)
	}
	return v, nil
}

// StrategyPosition returns the signed share count for (strategyID, symbol),
// 0 when absent (types.py:74-75 [MUST-MATCH]).
func (a AccountSnapshot) StrategyPosition(strategyID, symbol string) Qty {
	return a.Positions[StrategySymbol{StrategyID: strategyID, Symbol: symbol}]
}

// NetPositionAcrossStrategies sums all strategies' signed positions in the
// symbol (types.py:77-79 [MUST-MATCH]).
func (a AccountSnapshot) NetPositionAcrossStrategies(symbol string) (Qty, error) {
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
// non-zero positions (types.py:81-94 [MUST-MATCH]). Symbols missing from
// LastClose contribute 0 — positions with unknown price are invisible to the
// budget, exactly as in the reference.
func (a AccountSnapshot) GrossExposureForStrategy(strategyID string) (Money, error) {
	var total Money
	for key, qty := range a.Positions {
		if key.StrategyID != strategyID || qty == 0 {
			continue
		}
		price, ok := a.LastClose[key.Symbol]
		if !ok || price == 0 {
			continue // contributes Decimal(0) in the reference
		}
		absQty, err := qty.Abs()
		if err != nil {
			return 0, fmt.Errorf("gross exposure for %s/%s: %w", strategyID, key.Symbol, err)
		}
		// Python computes abs(Decimal(qty)) * price — the contribution keeps
		// the price's sign (only relevant for a nonsensical negative close;
		// preserved exactly for parity).
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

// positionEntryJSON flattens the tuple-keyed positions map for JSON (Go maps
// with struct keys cannot marshal natively; the Python tuple-keyed dict is
// not JSON-serializable either, so this defines the canonical encoding).
type positionEntryJSON struct {
	StrategyID string `json:"strategy_id"`
	Symbol     string `json:"symbol"`
	Qty        Qty    `json:"qty"`
}

type accountSnapshotJSON struct {
	NAV                Money               `json:"nav"`
	Cash               Money               `json:"cash"`
	RealizedPnLToday   Money               `json:"realized_pnl_today"`
	UnrealizedPnLToday Money               `json:"unrealized_pnl_today"`
	Positions          []positionEntryJSON `json:"positions"`
	LastClose          map[string]Price    `json:"last_close"`
}

// MarshalJSON encodes the snapshot with positions as a deterministic array
// sorted by (strategy_id, symbol).
func (a AccountSnapshot) MarshalJSON() ([]byte, error) {
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
	return json.Marshal(accountSnapshotJSON{
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
func (a *AccountSnapshot) UnmarshalJSON(b []byte) error {
	if string(bytes.TrimSpace(b)) == "null" {
		return nil
	}
	var raw accountSnapshotJSON
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
	*a = AccountSnapshot{
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
