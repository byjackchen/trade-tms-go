package sepa

// config.go is the SEPA SignalGenerator configuration and its validation.
//
// equity_provider is an injected runtime closure returning current account
// equity as a decimal-valued float, pulled at sizing time. We model it as
// func() float64: the only use is float(equity_provider()), so a float64
// provider is exact and avoids dragging a Decimal type through the pure layer.
// The closure is pulled at sizing time, never cached.

import (
	"errors"
	"fmt"
)

// Config is the strategy-level SEPA config. All fields are required (no
// defaults); the engine injects symbol + equityProvider per-instrument and
// resolves the tunable knobs from internal/params.
type Config struct {
	Symbol                 string
	EquityProvider         func() float64
	RiskPct                float64
	MarketCapMinUSD        float64
	HardStopPct            float64
	PivotBufferPct         float64
	BreakoutVolumeMultiple float64
	VCPLookback            int
	HistoryMaxBars         int
	Timezone               string
}

// ErrInvalidConfig wraps every config-validation failure so callers can test
// errors.Is. The messages embed the offending field name as a stable substring
// ("equity_provider"/"risk_pct"/"hard_stop_pct").
var ErrInvalidConfig = errors.New("sepa: invalid config")

// Validate checks each config field:
//
//   - equity_provider must be callable (here: non-nil) -> message contains
//     "equity_provider".
//   - 0 < risk_pct <= 100, else message contains "risk_pct".
//   - hard_stop_pct > 0, else message contains "hard_stop_pct".
//
// Validation NEVER calls equity_provider() (the closure may not be ready at
// construction); we only nil-check it.
func (c Config) Validate() error {
	if c.EquityProvider == nil {
		return fmt.Errorf("%w: equity_provider must be a callable returning Decimal", ErrInvalidConfig)
	}
	if !(c.RiskPct > 0 && c.RiskPct <= 100) {
		return fmt.Errorf("%w: risk_pct must be in (0, 100]", ErrInvalidConfig)
	}
	if !(c.HardStopPct > 0) {
		return fmt.Errorf("%w: hard_stop_pct must be > 0", ErrInvalidConfig)
	}
	return nil
}
