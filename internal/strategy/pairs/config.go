package pairs

// config.go: PairsSignalGeneratorConfig and its construction-time validation
// (spec §4).

import (
	"errors"
	"fmt"
)

// EquityProvider returns LIVE account equity in USD as a float64. It is called
// at SIZING TIME on every entry, never cached. A float64 provider is exact for
// the magnitudes used.
type EquityProvider func() float64

// Config is the immutable Pairs configuration (spec §4.1).
// All fields are required; defaults come from the JSON param file (spec §5),
// resolved upstream by internal/params.
type Config struct {
	// EquityProvider supplies live account equity; required, never nil.
	EquityProvider EquityProvider
	// Pairs is the pair universe (config order is preserved everywhere).
	Pairs []Pair
	// Lookback is the rolling window for BOTH the OLS hedge ratio and the
	// spread mean/std (bars). Must be >= 5.
	Lookback int
	// EntryZ is the absolute z-score entry threshold (> 0).
	EntryZ float64
	// ExitZ is the absolute z-score exit threshold (>= 0, < EntryZ).
	ExitZ float64
	// CapitalPerPairPct is the fraction of equity allocated per pair, in (0,1].
	CapitalPerPairPct float64
	// Timezone is declared/persisted only; NEVER used in signal math (§11).
	Timezone string
}

// Validate runs the construction-time checks in order, with substring-stable
// messages (spec §4.2). It returns errors rather than panicking. Rule 1
// (provider not callable) is represented by a nil EquityProvider and wraps a
// distinct sentinel so callers can distinguish the type error from the value
// errors; the provider is NOT invoked during validation.
func (c Config) Validate() error {
	if c.EquityProvider == nil {
		return fmt.Errorf("%w: equity_provider must be a callable returning Decimal", ErrConfigType)
	}
	if len(c.Pairs) == 0 {
		return errors.New("pairs must not be empty")
	}
	if c.Lookback < 5 {
		return errors.New("lookback must be >= 5")
	}
	if c.EntryZ <= 0 || c.ExitZ < 0 {
		return errors.New("entry_z must be > 0 and exit_z must be >= 0")
	}
	if c.ExitZ >= c.EntryZ {
		return errors.New("exit_z must be < entry_z (else no entry/exit gap)")
	}
	if !(c.CapitalPerPairPct > 0 && c.CapitalPerPairPct <= 1) {
		return errors.New("capital_per_pair_pct must be in (0, 1]")
	}
	return nil
}

// ErrConfigType flags a non-callable (nil) equity_provider. Value errors
// (rules 2-6) are plain errors, preserving the type/value distinction (spec §4.2).
var ErrConfigType = errors.New("pairs config type error")
