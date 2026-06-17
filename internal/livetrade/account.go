package livetrade

// account.go adapts internal/accounting.Account to the moomoo.AccountBook the
// MoomooExecutor settles broker fills into and reads net positions from. It is
// the SAME accounting engine a backtest uses (NETTING positions + realized PnL),
// so a paper/live fill settles identically to a simulated one — the property
// that makes "green on the mock venue" predict "green on a real account".
//
// Threading: the executor calls ApplyFill from the client's reader goroutine and
// Position from the strategy loop; accounting.Account is not internally locked,
// so this adapter serialises both behind a mutex. The dispatch loop's own reads
// (via the gated submitter NetPosition path) also go through here.

import (
	"sync"

	"github.com/byjackchen/trade-tms-go/internal/accounting"
	"github.com/byjackchen/trade-tms-go/internal/domain"
	moexec "github.com/byjackchen/trade-tms-go/internal/exec/moomoo"
)

// AccountAdapter wraps an *accounting.Account behind a mutex and exposes the
// narrow moomoo.AccountBook surface (ApplyFill + Position). It also serves the
// gated submitter's snapshot reads (Snapshot / NetPosition / LastPrice) so the
// pre-submit gate sees the up-to-date book.
type AccountAdapter struct {
	mu   sync.Mutex
	acct *accounting.Account
}

// NewAccountAdapter wraps acct (must be non-nil).
func NewAccountAdapter(acct *accounting.Account) *AccountAdapter {
	return &AccountAdapter{acct: acct}
}

// ApplyFill settles one broker fill and returns the resulting position snapshot
// (moomoo.AccountBook). The FillOutcome accounting computes is dropped — the
// executor does not need it (PnL aggregates live in the account).
func (a *AccountAdapter) ApplyFill(f domain.Fill) (domain.Position, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	pos, _, err := a.acct.ApplyFill(f)
	return pos, err
}

// Position returns the (strategy, symbol) net position snapshot; ok=false if it
// has never been opened (moomoo.AccountBook).
func (a *AccountAdapter) Position(strategyID, symbol string) (domain.Position, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.acct.Position(strategyID, symbol)
}

// OpenPositions returns snapshots of every non-flat (strategy, symbol) position
// in deterministic (strategy, symbol) order (moomoo.AccountBook). The flatten
// path enumerates these to close each originating BOOK row under its OWN
// strategy id so the fill nets it to 0 -> CLOSED (no phantom FLATTEN rows).
func (a *AccountAdapter) OpenPositions() []domain.Position {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.acct.OpenPositions()
}

// ObserveBar records a symbol's last price so the gate's estimated-fill price +
// the health snapshot's mark-to-market are current. Called by the trade session
// on every bar (mirroring the engine's ObserveBar before strategies run).
func (a *AccountAdapter) ObserveBar(bar domain.Bar) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.acct.ObserveBar(bar)
}

// Snapshot returns the domain account snapshot the portfolio gate's budget /
// concentration / single-name rules read. It uses the accounting engine's
// parity snapshot (NAV = settled cash; day P&L 0) for the BUDGET rules so live
// gating matches backtest gating exactly. The daily-loss-halt rule reads the
// MarkedSnapshot instead (see MarkedSnapshot).
func (a *AccountAdapter) Snapshot() (domain.PortfolioSnapshot, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.acct.Snapshot()
}

// MarkedSnapshot returns a LIVE snapshot with day P&L marked to market — the
// authoritative input for the daily-loss-halt rule (which is DORMANT in the
// parity Snapshot, where RealizedPnLToday/UnrealizedPnLToday are 0 for backtest
// parity). Day P&L = realized + unrealized vs the session's opening NAV; NAV =
// current equity (cash + unrealized). startingNAV is the session's opening
// balance (the day's baseline). This is the live extension the daily-loss halt
// needs: a backtest never crosses it (P&L 0), a live session does when a held
// position moves against the book.
func (a *AccountAdapter) MarkedSnapshot(startingNAV domain.Money) (domain.PortfolioSnapshot, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	base, err := a.acct.Snapshot()
	if err != nil {
		return domain.PortfolioSnapshot{}, err
	}
	realized := a.acct.RealizedPnL()
	unrealized, err := a.acct.Unrealized()
	if err != nil {
		return domain.PortfolioSnapshot{}, err
	}
	equity, err := a.acct.Equity()
	if err != nil {
		return domain.PortfolioSnapshot{}, err
	}
	// Day P&L baseline = the session's opening NAV. realized is cumulative since
	// the session start (which restarts daily for a live node), so realized today
	// == realized; unrealized today == current unrealized.
	_ = startingNAV
	return domain.NewPortfolioSnapshot(
		equity, base.Cash, realized, unrealized, base.Positions, base.LastClose,
	), nil
}

// LastPrice returns the last observed price for symbol (estimated-fill price for
// the gate); ok=false when unknown (gate treats it as 0).
func (a *AccountAdapter) LastPrice(symbol string) (domain.Price, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.acct.LastPrice(symbol)
}

// NetPositionAcrossStrategies returns the signed net position in symbol across
// all strategies (the venue-net convention strategies size FLAT closes against).
func (a *AccountAdapter) NetPositionAcrossStrategies(symbol string) domain.Qty {
	snap, err := a.Snapshot()
	if err != nil {
		return 0
	}
	net, err := snap.NetPositionAcrossStrategies(symbol)
	if err != nil {
		return 0
	}
	return net
}

// compile-time check: the adapter is a valid AccountBook for the executor.
var _ moexec.AccountBook = (*AccountAdapter)(nil)
