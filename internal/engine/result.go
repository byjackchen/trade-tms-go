package engine

// result.go is the structured backtest result the assembler returns, plus the
// run configuration. The result carries the database-source-of-truth fields
// (final balance, PnL, equity curves, trades, positions) and enough detail to
// emit the legacy runs/{ts}/*.json artifacts (locked decision 4).

import (
	"time"

	"github.com/byjackchen/trade-tms-go/internal/accounting"
	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// FillProfile selects the executor fill model.
type FillProfile string

const (
	// ProfileNautilusCompat is the zero-cost parity gate profile: same-bar
	// close fill, no slippage, no commission.
	ProfileNautilusCompat FillProfile = "nautilus-compat"
	// ProfileRealistic is the production default: next-bar-open fill with
	// configurable slippage and commission.
	ProfileRealistic FillProfile = "realistic"
)

// IsValid reports whether p is a known profile.
func (p FillProfile) IsValid() bool {
	return p == ProfileNautilusCompat || p == ProfileRealistic
}

// StrategySpec describes one scripted strategy to run: its engine id and its
// intent list.
type StrategySpec struct {
	ID      string
	Intents []Intent
}

// Config is the backtest specification.
type Config struct {
	// Tickers in registration order. Same-timestamp bars dispatch in this order
	// (locked decision 2).
	Tickers []string
	// Start, End bound the bar window (inclusive).
	Start, End calendar.Date
	// StartingBalance seeds the account (USD).
	StartingBalance domain.Money
	// Profile selects the fill model. Defaults to nautilus-compat when empty.
	Profile FillProfile
	// Realistic configures the realistic model (ignored for nautilus-compat).
	Realistic RealisticParams
	// Strategies are the scripted strategies (the parity drivers).
	Strategies []StrategySpec
	// Progress, when non-nil, is called after each bar is dispatched with the
	// number of bars processed so far and the total scheduled (for "% complete"
	// reporting). It runs on the loop goroutine; keep it cheap and non-blocking.
	// It MUST NOT mutate engine state. Optional.
	Progress func(processed, total int)
}

// RealisticParams configures the realistic fill model.
type RealisticParams struct {
	SlippageBps        float64
	CommissionPerShare domain.Money
	CommissionBps      float64
}

// Result is the deterministic backtest output.
type Result struct {
	// StartingBalance and FinalBalance in USD; PnL = Final - Starting.
	StartingBalance domain.Money
	FinalBalance    domain.Money
	TotalPnL        domain.Money

	// Profile is the fill model used.
	Profile FillProfile

	// Strategies are the engine strategy ids run, sorted.
	Strategies []string

	// Orders submitted (submission order), Fills (settlement order).
	Orders []domain.Order
	Fills  []domain.Fill

	// Positions are the final position snapshots (all keys, sorted).
	Positions []domain.Position

	// AccountStates is the account-state curve (one point per settlement plus
	// the initial state), the basis for account.json (§7.4).
	AccountStates []AccountStatePoint

	// TotalEquityCurve is the per-bar sampled account equity.
	TotalEquityCurve []accounting.EquityPoint

	// StrategyEquity maps engine strategy id -> per-bar cumulative-PnL curve.
	StrategyEquity map[string][]accounting.EquityPoint

	// BarsProcessed counts dispatched bars; SampledDays counts equity samples.
	BarsProcessed int
	SampledDays   int

	// FirstTS/LastTS bound the processed data (UTC).
	FirstTS, LastTS time.Time
}
