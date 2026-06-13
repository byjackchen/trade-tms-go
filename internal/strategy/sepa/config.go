package sepa

// config.go is the SEPA SignalGenerator configuration, mirroring
// SEPASignalGeneratorConfig (sepa/signal.py:104-127) and its __post_init__
// validation (signal.py:155-164) EXACTLY.
//
// equity_provider is an injected runtime closure returning current account
// equity as a decimal-valued float (the reference pulls Decimal and float()s
// it at sizing time, signal.py:316). We model it as func() float64: the
// reference's only use of the Decimal is float(equity_provider()), so a
// float64 provider is byte-identical and avoids dragging a Decimal type
// through the pure layer. The closure is pulled at sizing time, never cached.

import (
	"errors"
	"fmt"
)

// Config is the strategy-level SEPA config. All fields are required (the
// reference has no defaults); the engine injects symbol + equityProvider
// per-instrument and resolves the tunable knobs from internal/params.
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
// errors.Is. The messages embed the offending field name, matching the
// reference's TypeError/ValueError message substrings
// (test_signal.py asserts match="equity_provider"/"risk_pct"/"hard_stop_pct").
var ErrInvalidConfig = errors.New("sepa: invalid config")

// Validate mirrors SEPASignalGeneratorConfig.__post_init__ (signal.py:155-164):
//
//   - equity_provider must be callable (here: non-nil) -> message contains
//     "equity_provider".
//   - 0 < risk_pct <= 100, else message contains "risk_pct".
//   - hard_stop_pct > 0, else message contains "hard_stop_pct".
//
// The reference NEVER calls equity_provider() during validation (the closure
// may not be ready at construction); we likewise only nil-check it.
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
