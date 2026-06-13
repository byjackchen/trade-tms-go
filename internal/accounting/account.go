package accounting

// account.go is the position book + margin account. It owns the per-(strategy,
// symbol) Position map, applies fills (updating realized PnL and the cash
// balance), tracks the last seen price per symbol for mark-to-market, and emits
// an AccountState on every settlement via the message bus.
//
// Determinism: although positions live in a map, every ordered traversal
// (snapshots, equity aggregation) sorts its keys, so output never depends on
// Go's map iteration order.

import (
	"fmt"
	"sort"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/core"
	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// Account is the base-currency (USD) margin account plus position book. Cash
// (Total balance) = startingBalance + cumulative realized PnL. The zero-margin
// equity instrument keeps Free == Total (no locked margin). Not safe for
// concurrent use.
type Account struct {
	startingBalance domain.Money
	realized        domain.Money // cumulative realized PnL across all positions

	positions map[domain.StrategySymbol]*Position
	lastPrice map[string]domain.Price

	bus *core.MsgBus
}

// NewAccount returns an account seeded with startingBalance. bus may be nil
// (no observers); when non-nil, an AccountState is published on each fill. The
// caller (engine assembler) is responsible for publishing the INITIAL
// AccountState at the starting balance before the first fill, mirroring the
// Nautilus venue's run-start event.
func NewAccount(startingBalance domain.Money, bus *core.MsgBus) *Account {
	return &Account{
		startingBalance: startingBalance,
		positions:       make(map[domain.StrategySymbol]*Position),
		lastPrice:       make(map[string]domain.Price),
		bus:             bus,
	}
}

// StartingBalance returns the seed balance.
func (a *Account) StartingBalance() domain.Money { return a.startingBalance }

// RealizedPnL returns cumulative realized PnL across all positions.
func (a *Account) RealizedPnL() domain.Money { return a.realized }

// Cash returns the current base-currency balance = starting + realized.
func (a *Account) Cash() (domain.Money, error) {
	v, err := a.startingBalance.Add(a.realized)
	if err != nil {
		return 0, fmt.Errorf("account cash: %w", err)
	}
	return v, nil
}

// LastPrice returns the last seen price for symbol and whether one exists.
func (a *Account) LastPrice(symbol string) (domain.Price, bool) {
	p, ok := a.lastPrice[symbol]
	return p, ok
}

// ObserveBar records the bar's close as the symbol's last price for
// mark-to-market. Called by the engine on each bar before fills settle.
func (a *Account) ObserveBar(bar domain.Bar) { a.lastPrice[bar.Symbol] = bar.Close }

// position returns the (strategy, symbol) position, creating a flat one on
// first use.
func (a *Account) position(strategyID, symbol string) *Position {
	key := domain.StrategySymbol{StrategyID: strategyID, Symbol: symbol}
	p, ok := a.positions[key]
	if !ok {
		p = NewPosition(strategyID, symbol)
		a.positions[key] = p
	}
	return p
}

// Position returns a snapshot of the (strategy, symbol) position; the second
// result is false when no position has ever been opened for that key.
func (a *Account) Position(strategyID, symbol string) (domain.Position, bool) {
	key := domain.StrategySymbol{StrategyID: strategyID, Symbol: symbol}
	p, ok := a.positions[key]
	if !ok {
		return domain.Position{}, false
	}
	return p.Snapshot(), true
}

// ApplyFill settles a fill: it routes the fill to its (strategy, symbol)
// position, moves the cash balance by the realized delta, refreshes the
// symbol's last price to the fill price, and publishes an AccountState (cadence:
// one per fill, matching Nautilus). Returns the position's resulting snapshot
// and the fill outcome.
func (a *Account) ApplyFill(f domain.Fill) (domain.Position, FillOutcome, error) {
	if err := f.Validate(); err != nil {
		return domain.Position{}, FillOutcome{}, fmt.Errorf("account apply fill: %w", err)
	}
	pos := a.position(f.StrategyID, f.Symbol)
	out, err := pos.ApplyFill(f)
	if err != nil {
		return domain.Position{}, FillOutcome{}, err
	}
	if out.RealizedDelta != 0 {
		acc, aerr := a.realized.Add(out.RealizedDelta)
		if aerr != nil {
			return domain.Position{}, FillOutcome{}, fmt.Errorf("account realized: %w", aerr)
		}
		a.realized = acc
	}
	// A fill establishes a traded price for the symbol.
	a.lastPrice[f.Symbol] = f.Price

	if err := a.emitAccountState(f.TS); err != nil {
		return domain.Position{}, FillOutcome{}, err
	}
	return pos.Snapshot(), out, nil
}

// emitAccountState publishes the post-settlement balance on the bus.
func (a *Account) emitAccountState(ts time.Time) error {
	cash, err := a.Cash()
	if err != nil {
		return err
	}
	if a.bus != nil {
		a.bus.PublishAccountState(core.AccountState{
			TS:       ts,
			Total:    cash,
			Free:     cash, // zero-margin equity: free == total
			Locked:   0,
			Realized: a.realized,
		})
	}
	return nil
}

// EmitInitialState publishes the starting-balance AccountState at ts (the
// engine calls this once before the run, mirroring the Nautilus venue's
// run-start event).
func (a *Account) EmitInitialState(ts time.Time) error { return a.emitAccountState(ts) }

// Unrealized returns total unrealized PnL across all open positions, marked at
// each symbol's last seen price. Positions whose symbol has no last price
// contribute 0 (they cannot be marked).
func (a *Account) Unrealized() (domain.Money, error) {
	var total domain.Money
	for _, key := range a.sortedKeys() {
		p := a.positions[key]
		if p.IsFlat() {
			continue
		}
		last, ok := a.lastPrice[p.Symbol()]
		if !ok {
			continue
		}
		u, err := p.UnrealizedPnL(last)
		if err != nil {
			return 0, err
		}
		total, err = total.Add(u)
		if err != nil {
			return 0, fmt.Errorf("account unrealized: %w", err)
		}
	}
	return total, nil
}

// Equity returns cash + total unrealized PnL.
func (a *Account) Equity() (domain.Money, error) {
	cash, err := a.Cash()
	if err != nil {
		return 0, err
	}
	un, err := a.Unrealized()
	if err != nil {
		return 0, err
	}
	eq, err := cash.Add(un)
	if err != nil {
		return 0, fmt.Errorf("account equity: %w", err)
	}
	return eq, nil
}

// sortedKeys returns the position keys in deterministic (strategy, symbol)
// order.
func (a *Account) sortedKeys() []domain.StrategySymbol {
	keys := make([]domain.StrategySymbol, 0, len(a.positions))
	for k := range a.positions {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].StrategyID != keys[j].StrategyID {
			return keys[i].StrategyID < keys[j].StrategyID
		}
		return keys[i].Symbol < keys[j].Symbol
	})
	return keys
}

// OpenPositions returns snapshots of all non-flat positions, sorted by
// (strategy, symbol).
func (a *Account) OpenPositions() []domain.Position {
	var out []domain.Position
	for _, key := range a.sortedKeys() {
		if p := a.positions[key]; !p.IsFlat() {
			out = append(out, p.Snapshot())
		}
	}
	return out
}

// AllPositions returns snapshots of every position ever opened (including flat
// ones), sorted by (strategy, symbol).
func (a *Account) AllPositions() []domain.Position {
	out := make([]domain.Position, 0, len(a.positions))
	for _, key := range a.sortedKeys() {
		out = append(out, a.positions[key].Snapshot())
	}
	return out
}

// Snapshot builds a domain.AccountSnapshot for the risk pipeline. Per the
// reference glue (spec §2.9), Cash is set equal to NAV (= equity) and the
// today P&L fields default to 0 (daily-loss-halt dormant in backtest). The
// positions map carries signed quantities; last_close carries the last seen
// price per symbol.
func (a *Account) Snapshot() (domain.AccountSnapshot, error) {
	equity, err := a.Equity()
	if err != nil {
		return domain.AccountSnapshot{}, err
	}
	positions := make(map[domain.StrategySymbol]domain.Qty, len(a.positions))
	for key, p := range a.positions {
		if p.IsFlat() {
			continue // skip flat (signed == 0), matching the reference glue
		}
		positions[key] = p.SignedQty()
	}
	lastClose := make(map[string]domain.Price, len(a.lastPrice))
	for k, v := range a.lastPrice {
		lastClose[k] = v
	}
	return domain.NewAccountSnapshot(equity, equity, 0, 0, positions, lastClose), nil
}
