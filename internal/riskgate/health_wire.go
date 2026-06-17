package riskgate

// health_wire.go exposes the PortfolioHealthSnapshot's exact-decimal ratio
// fields as float64 for the live publish path. The PortfolioHealthUpdate WS /
// Redis-stream wire carries floats (api-ws-redis.md §5.11; reference
// src/data/custom_data.py:241-268), so the live publisher reads these
// accessors and emits floats; the REST endpoint stringifies the wire value.
//
// These are read-only projections (Float64 == nearest float64 of the exact
// dec). The DailyLossHalt bool is already exported on the struct.

// DayPnLFloat returns the realized+unrealized day P&L as a float64 (USD).
func (s PortfolioHealthSnapshot) DayPnLFloat() float64 { return s.DayPnL.Float64() }

// DayPnLPctFloat returns day_pnl/nav as a float64 (0 when nav <= 0).
func (s PortfolioHealthSnapshot) DayPnLPctFloat() float64 { return s.DayPnLPct.Float64() }

// HaltHeadroomPctFloat returns (day_pnl - threshold)/nav, clamped to 0 when
// halted, as a float64.
func (s PortfolioHealthSnapshot) HaltHeadroomPctFloat() float64 { return s.HaltHeadroomPct.Float64() }

// ConcentrationPctFloat returns the largest single-symbol net exposure / nav as
// a float64.
func (s PortfolioHealthSnapshot) ConcentrationPctFloat() float64 {
	return s.ConcentrationPct.Float64()
}

// IsDailyLossHalt reports whether day P&L is strictly below the -halt_pct*NAV
// threshold (informational in signal mode, decision 6).
func (s PortfolioHealthSnapshot) IsDailyLossHalt() bool { return s.DailyLossHalt }
