package riskgate

// context_types.go defines the published context-update value types (spec §7.8)
// plus the shared regime label constants (§7.2). These are the message-bus
// payloads the live context publishers emit (regime / fundamentals / earnings).
// In backtest the engine reads them off the ContextProvider per bar instead of a
// live bus, but the payload shapes are preserved so live wiring stays identical.

import "time"

// Regime labels — the four classification outputs of ComputeRegime (§7.2).
// RegimeNeutral is the cold-start / insufficient-history default of
// SharedContextState.regime.
const (
	RegimeBull    = "bull"
	RegimeBear    = "bear"
	RegimeNeutral = "neutral"
	RegimeWarning = "warning"
)

// Context computation constants (§7.2/§9).
const (
	regimeMinBars        = 200 // bars required for a 200d MA + slope
	regimeSlopeWindow    = 30  // MA slope lookback
	regimeSlopeFlatPct   = 0.0 // slope > this -> bull, else warning
	earningsBlackoutDays = 5   // ± calendar-day blackout window
	sf1DimensionDefault  = "MRT"
)

// RegimeUpdate is the market-regime payload published by the regime provider
// (§7.8). Value is one of RegimeBull/Bear/Neutral/Warning. TSEvent/TSInit are
// the triggering SPY bar's event timestamp.
type RegimeUpdate struct {
	Value   string
	TSEvent time.Time
	TSInit  time.Time
}

// MarketCapUpdate is the per-ticker market-cap payload (§7.8). Value is USD as a
// float64 on the wire; the exact decimal is carried separately in shared state.
// ValueDec is the internal exact value (spec §7.6: keep float64 on the wire,
// carry the decimal alongside for the API layer).
type MarketCapUpdate struct {
	Ticker   string
	Value    float64
	ValueDec dec
	TSEvent  time.Time
	TSInit   time.Time
}

// EarningsBlackoutUpdate is the per-ticker blackout payload (§7.8). Value is
// true while as_of is within
// ±blackout_days of any reported earnings date.
type EarningsBlackoutUpdate struct {
	Ticker  string
	Value   bool
	TSEvent time.Time
	TSInit  time.Time
}
