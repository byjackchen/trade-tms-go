package orb

// config.go is the ORB (intraday_breakout) SignalGenerator configuration,
// mirroring IntradayBreakoutSignalGeneratorConfig (intraday_breakout/signal.py
// :51-90) and its __post_init__ validation EXACTLY — including every error
// message substring the Python tests anchor with pytest.raises(match=...)
// (test_signal.py:120-176): "equity_provider", "risk_pct", "range_minutes",
// "vol_multiple", "profit_target_r", "hard_stop_pct", "eod_exit_time",
// "timezone".

import (
	"errors"
	"fmt"
	"time"
)

// Session open is hard-coded 09:30 exchange-local (signal.py:47-48). ORB is
// unambiguously a US-equities pattern; other venues need a different template.
const (
	sessionOpenHour   = 9
	sessionOpenMinute = 30
)

// Config is the strategy-level ORB config. symbol + equityProvider are injected
// per-instrument by the engine; the tunable knobs are resolved from
// internal/params (defaults: risk_pct 1.0, range_minutes 30, vol_multiple 1.5,
// profit_target_r 2.0, hard_stop_pct 1.0, eod_exit_time "15:55", timezone
// "America/New_York").
//
// EquityProvider returns current account equity. The Python reference pulls a
// Decimal and float()s it at sizing time; modelling it as func() float64 is
// byte-identical for every use (sizing math and the equity_at_snapshot field)
// and avoids dragging a Decimal through the pure layer. Never cached: it is
// invoked at every entry and at every state_dict() call.
type Config struct {
	Symbol         string
	EquityProvider func() float64
	RiskPct        float64
	RangeMinutes   int
	VolMultiple    float64
	ProfitTargetR  float64
	HardStopPct    float64
	EODExitTime    string // "HH:MM" exchange-local
	Timezone       string // IANA tz name
}

// DefaultConfig returns a Config preloaded with the baseline-JSON defaults
// (intraday_breakout.json). Symbol and EquityProvider must still be set.
func DefaultConfig() Config {
	return Config{
		RiskPct:       1.0,
		RangeMinutes:  30,
		VolMultiple:   1.5,
		ProfitTargetR: 2.0,
		HardStopPct:   1.0,
		EODExitTime:   "15:55",
		Timezone:      "America/New_York",
	}
}

// ErrInvalidConfig wraps every config-validation failure so callers can use
// errors.Is; the message embeds the offending field name to match the
// reference's TypeError/ValueError substrings.
var ErrInvalidConfig = errors.New("orb: invalid config")

// Validate mirrors IntradayBreakoutSignalGeneratorConfig.__post_init__
// (signal.py:65-90) in order. The reference NEVER calls equity_provider()
// during validation; we likewise only nil-check it.
func (c Config) Validate() error {
	if c.EquityProvider == nil {
		return fmt.Errorf("%w: equity_provider must be a callable returning Decimal", ErrInvalidConfig)
	}
	if !(c.RiskPct > 0 && c.RiskPct <= 100) {
		return fmt.Errorf("%w: risk_pct must be in (0, 100]", ErrInvalidConfig)
	}
	if c.RangeMinutes < 1 {
		return fmt.Errorf("%w: range_minutes must be >= 1", ErrInvalidConfig)
	}
	if !(c.VolMultiple > 0) {
		return fmt.Errorf("%w: vol_multiple must be > 0", ErrInvalidConfig)
	}
	if !(c.ProfitTargetR > 0) {
		return fmt.Errorf("%w: profit_target_r must be > 0", ErrInvalidConfig)
	}
	if !(c.HardStopPct > 0 && c.HardStopPct <= 50) {
		return fmt.Errorf("%w: hard_stop_pct must be in (0, 50]", ErrInvalidConfig)
	}
	if _, _, err := parseHHMM(c.EODExitTime); err != nil {
		return fmt.Errorf("%w: eod_exit_time must be HH:MM", ErrInvalidConfig)
	}
	if _, err := time.LoadLocation(c.Timezone); err != nil {
		return fmt.Errorf("%w: timezone %q not recognized", ErrInvalidConfig, c.Timezone)
	}
	return nil
}

// parseHHMM splits "HH:MM" and validates 0<=H<=23, 0<=M<=59, matching the
// reference's int(h_str)/int(m_str) + range check (signal.py:79-85).
func parseHHMM(s string) (h, m int, err error) {
	var ok bool
	h, m, ok = splitColonInts(s)
	if !ok {
		return 0, 0, fmt.Errorf("%w: eod_exit_time must be HH:MM", ErrInvalidConfig)
	}
	if !(h >= 0 && h <= 23 && m >= 0 && m <= 59) {
		return 0, 0, fmt.Errorf("%w: eod_exit_time must be HH:MM", ErrInvalidConfig)
	}
	return h, m, nil
}

// splitColonInts parses exactly "<int>:<int>" with no surrounding spaces,
// rejecting "noon" / "25" / "1:2:3" / empty parts (mirrors str.split(":") with
// a 2-tuple unpack + int()).
func splitColonInts(s string) (a, b int, ok bool) {
	idx := -1
	for i := 0; i < len(s); i++ {
		if s[i] == ':' {
			if idx >= 0 {
				return 0, 0, false // more than one colon -> unpack would fail
			}
			idx = i
		}
	}
	if idx < 0 {
		return 0, 0, false
	}
	a, ok1 := atoiStrict(s[:idx])
	b, ok2 := atoiStrict(s[idx+1:])
	if !ok1 || !ok2 {
		return 0, 0, false
	}
	return a, b, true
}

// atoiStrict parses a base-10 (optionally signed) integer with no whitespace,
// matching Python int(str) on a split component. Empty -> not ok.
func atoiStrict(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	neg := false
	i := 0
	if s[0] == '+' || s[0] == '-' {
		neg = s[0] == '-'
		i = 1
		if i == len(s) {
			return 0, false
		}
	}
	n := 0
	for ; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		n = n*10 + int(s[i]-'0')
	}
	if neg {
		n = -n
	}
	return n, true
}
