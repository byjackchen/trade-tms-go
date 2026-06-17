package riskgate

// context_state.go (spec §7.1): the single mutable context store consulted by
// every strategy runner — market regime, per-ticker market cap, per-ticker
// earnings-blackout.
//
// The system is concurrent, so we guard with a sync.RWMutex (last-writer-wins
// per field, sole-writer-per-field convention §7.4). Snapshot returns a deep
// copy so readers never observe a torn map mid-mutation.

import (
	"sync"
)

// SharedContextState is the mutable context store. regime defaults to
// "neutral"; both maps default empty. Construct via NewSharedContextState —
// each backtest run owns its own instance for isolation.
type SharedContextState struct {
	mu               sync.RWMutex
	regime           string
	marketCap        map[string]dec
	earningsBlackout map[string]bool
}

// NewSharedContextState returns a state with the defaults: regime "neutral",
// empty maps.
func NewSharedContextState() *SharedContextState {
	return &SharedContextState{
		regime:           RegimeNeutral,
		marketCap:        map[string]dec{},
		earningsBlackout: map[string]bool{},
	}
}

// Regime returns the current regime label.
func (s *SharedContextState) Regime() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.regime
}

// SetRegime sets the regime label (RegimeActor is the sole writer, §7.4).
func (s *SharedContextState) SetRegime(regime string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.regime = regime
}

// MarketCap returns the latest market cap for ticker and whether it is known.
func (s *SharedContextState) MarketCap(ticker string) (dec, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.marketCap[ticker]
	return v, ok
}

// SetMarketCap sets a ticker's market cap (FundamentalsActor sole writer, §7.4).
func (s *SharedContextState) SetMarketCap(ticker string, value dec) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.marketCap[ticker] = value
}

// ReplaceMarketCap replaces the whole market-cap map (wholesale replacement must
// work, test_context_state.py). The argument is copied defensively.
func (s *SharedContextState) ReplaceMarketCap(m map[string]dec) {
	s.mu.Lock()
	defer s.mu.Unlock()
	nm := make(map[string]dec, len(m))
	for k, v := range m {
		nm[k] = v
	}
	s.marketCap = nm
}

// MarketCapFloats returns a snapshot copy of every known market cap as float64
// (the value SignalGenerators consume via set_market_cap). For the backtest
// engine's per-bar context injection — a deep copy so callers never observe a
// torn map.
func (s *SharedContextState) MarketCapFloats() map[string]float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]float64, len(s.marketCap))
	for k, v := range s.marketCap {
		out[k] = v.Float64()
	}
	return out
}

// EarningsBlackouts returns a snapshot copy of the full blackout map (deep copy).
func (s *SharedContextState) EarningsBlackouts() map[string]bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]bool, len(s.earningsBlackout))
	for k, v := range s.earningsBlackout {
		out[k] = v
	}
	return out
}

// EarningsBlackout returns whether ticker is in blackout (absent key == false,
// §7.7 — consumers interpret absence as false).
func (s *SharedContextState) EarningsBlackout(ticker string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.earningsBlackout[ticker]
}

// SetEarningsBlackout sets a ticker's blackout flag (EarningsActor sole writer).
func (s *SharedContextState) SetEarningsBlackout(ticker string, value bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.earningsBlackout[ticker] = value
}

// ReplaceEarningsBlackout replaces the whole blackout map (copied defensively).
func (s *SharedContextState) ReplaceEarningsBlackout(m map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	nm := make(map[string]bool, len(m))
	for k, v := range m {
		nm[k] = v
	}
	s.earningsBlackout = nm
}
