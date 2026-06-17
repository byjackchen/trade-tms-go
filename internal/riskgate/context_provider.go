package riskgate

// context_provider.go is the BACKTEST data-equivalent of the Python Context
// Actors (RegimeActor / FundamentalsActor / EarningsActor, spec §7.5-§7.7).
//
// In live mode the Actors subscribe to the SPY heartbeat bar and publish
// RegimeUpdate / MarketCapUpdate / EarningsBlackoutUpdate onto the message bus,
// which the strategy SignalGenerators consume. In BACKTEST there is no bus: the
// engine instead consults a ContextProvider once per bar with the bar's date,
// gets the published-equivalent updates, and feeds them into SharedContextState
// (the same store strategies read). The ContextProvider holds the full SPY /
// SF1 / EVENTS history and recomputes via the pure context_refresher functions
// with as_of = the bar date, so it is look-ahead-safe by construction: only
// data with date <= as_of is ever consulted (§7.2-§7.4).
//
// This mirrors the Actors' transition/dedup publishing semantics so the
// sequence of published updates a backtest sees matches what a live run would
// have published bar-for-bar (RegimeActor: on transition + first; Fundamentals:
// per-ticker value change; Earnings: per-ticker transition + first observation).

import (
	"sort"
	"time"
)

// ContextProvider computes per-bar context from full history, look-ahead-safe.
// It is the backtest stand-in for the three Context Actors. Tickers is the
// tracked universe (the SEPA stock universe in production, §7.8); per-ticker
// updates are emitted in tracked order for determinism.
type ContextProvider struct {
	spyHistory []SPYBar // sorted ascending by date (regime input)
	sf1Rows    []SF1Row
	earnings   []EarningsRow
	tickers    []string
	dimension  string
	blackout   int

	// dedup state mirroring the Actors' publish gating.
	lastRegime       *string
	lastMarketCap    map[string]dec  // by value (FundamentalsActor §7.6)
	lastBlackoutSeen map[string]bool // key present == observed at least once
	lastBlackout     map[string]bool
}

// NewContextProvider builds a provider over the given history. spyHistory is
// sorted ascending by date defensively. dimension defaults to "MRT" and blackout
// to 5 (the reference defaults) when zero-valued.
func NewContextProvider(spyHistory []SPYBar, sf1Rows []SF1Row, earnings []EarningsRow, tickers []string, dimension string, blackoutDays int) *ContextProvider {
	hist := make([]SPYBar, len(spyHistory))
	copy(hist, spyHistory)
	sort.SliceStable(hist, func(i, j int) bool { return hist[i].Date.Before(hist[j].Date) })
	if dimension == "" {
		dimension = sf1DimensionDefault
	}
	if blackoutDays == 0 {
		blackoutDays = earningsBlackoutDays
	}
	tk := make([]string, len(tickers))
	copy(tk, tickers)
	return &ContextProvider{
		spyHistory:       hist,
		sf1Rows:          sf1Rows,
		earnings:         earnings,
		tickers:          tk,
		dimension:        dimension,
		blackout:         blackoutDays,
		lastMarketCap:    map[string]dec{},
		lastBlackoutSeen: map[string]bool{},
		lastBlackout:     map[string]bool{},
	}
}

// RegimeAt returns the regime classification as of date (look-ahead-safe;
// §7.2). Pure read — does not advance dedup state. The backtest engine can call
// this directly for a strategy's per-bar regime lookup.
func (p *ContextProvider) RegimeAt(date time.Time) string {
	return ComputeRegime(p.spyHistory, date)
}

// MarketCapAt returns the latest known market cap for ticker as of date and
// whether it is known (look-ahead-safe; §7.3). Pure read.
func (p *ContextProvider) MarketCapAt(ticker string, date time.Time) (dec, bool) {
	caps := LoadSF1MarketCaps(p.sf1Rows, date, p.dimension)
	v, ok := caps[ticker]
	return v, ok
}

// EarningsBlackoutAt returns whether ticker is in earnings blackout as of date
// (look-ahead-safe; §7.4). Pure read.
func (p *ContextProvider) EarningsBlackoutAt(ticker string, date time.Time) bool {
	cal := LoadEarningsCalendar(p.earnings, date, p.blackout)
	return cal[ticker]
}

// ContextUpdates bundles the published-equivalent updates produced on one bar.
type ContextUpdates struct {
	Regime    *RegimeUpdate            // non-nil only on a regime transition (incl. first)
	MarketCap []MarketCapUpdate        // per-ticker, only on value change
	Earnings  []EarningsBlackoutUpdate // per-ticker, on transition + first observation
}

// OnBar advances the provider for a SPY heartbeat bar at ts, writes the new
// context into state, and returns the updates that a live run would have
// PUBLISHED on this bar (mirroring the Actors' dedup/transition semantics,
// §7.5-§7.7). The returned updates let a backtest record the exact published
// sequence; the always-write-to-state behavior matches the Actors writing
// shared_state every qualifying bar.
//
// Regime (§7.5): only classifies/writes when >= 200 bars are available as of
// ts; writes shared_state.regime EVERY qualifying bar; publishes only on a
// change from the last published value (nil counts as different -> first
// classification always publishes).
//
// MarketCap (§7.6): per tracked ticker in order; writes shared state BEFORE the
// publish; publishes only when the value differs from the last published value.
//
// Earnings (§7.7): per tracked ticker in order; writes shared state
// UNCONDITIONALLY every bar; publishes on first observation per ticker OR on a
// transition.
func (p *ContextProvider) OnBar(state *SharedContextState, ts time.Time) ContextUpdates {
	date := dateOnly(ts)
	var upd ContextUpdates

	// --- regime (RegimeActor §7.5) ---
	available := 0
	for _, b := range p.spyHistory {
		if !dateOnly(b.Date).After(date) {
			available++
		}
	}
	if available >= regimeMinBars {
		regime := ComputeRegime(p.spyHistory, date)
		state.SetRegime(regime) // always write every qualifying bar
		if p.lastRegime == nil || *p.lastRegime != regime {
			r := regime
			upd.Regime = &RegimeUpdate{Value: regime, TSEvent: ts, TSInit: ts}
			p.lastRegime = &r
		}
	}

	// --- market cap (FundamentalsActor §7.6) ---
	caps := LoadSF1MarketCaps(p.sf1Rows, date, p.dimension)
	if len(caps) > 0 {
		for _, ticker := range p.tickers {
			v, ok := caps[ticker]
			if !ok {
				continue // absent -> skip silently
			}
			if prev, seen := p.lastMarketCap[ticker]; seen && prev.Cmp(v) == 0 {
				continue // unchanged by value
			}
			state.SetMarketCap(ticker, v) // written BEFORE publish
			upd.MarketCap = append(upd.MarketCap, MarketCapUpdate{
				Ticker: ticker, Value: v.Float64(), ValueDec: v, TSEvent: ts, TSInit: ts,
			})
			p.lastMarketCap[ticker] = v
		}
	}

	// --- earnings blackout (EarningsActor §7.7) ---
	if len(p.tickers) > 0 {
		cal := LoadEarningsCalendar(p.earnings, date, p.blackout)
		for _, ticker := range p.tickers {
			value := cal[ticker]                     // absent -> false
			state.SetEarningsBlackout(ticker, value) // unconditional every bar
			if !p.lastBlackoutSeen[ticker] || p.lastBlackout[ticker] != value {
				upd.Earnings = append(upd.Earnings, EarningsBlackoutUpdate{
					Ticker: ticker, Value: value, TSEvent: ts, TSInit: ts,
				})
				p.lastBlackout[ticker] = value
				p.lastBlackoutSeen[ticker] = true
			}
		}
	}

	return upd
}
