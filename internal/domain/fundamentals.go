package domain

// fundamentals.go defines the Fundamentals value object. The Python
// reference has no standalone dataclass (spec §2.13): fundamentals = the SF1
// market-cap pipeline, which carries the value as Decimal in shared state
// and as float in MarketCapUpdate / SignalGenerator.set_market_cap. This
// type makes that record explicit while preserving both bridges.

import (
	"fmt"
	"time"
)

// Fundamentals is one ticker's fundamentals record as consumed by the
// platform today: the latest SF1 market cap at or before AsOf.
//
//   - MarketCapUSD is exact Money (the Decimal side of the pipeline:
//     Decimal(str(marketcap)), spec §2.13.1).
//   - MarketCapFloat64 is the float bridge (MarketCapUpdate.value and
//     set_market_cap take plain floats).
//   - The cold-start default of 0 is conservatively blocking: SEPA trend
//     template rule 8 fails until the first publish [MUST-MATCH].
type Fundamentals struct {
	Ticker       string    `json:"ticker"`
	MarketCapUSD Money     `json:"market_cap_usd"`
	AsOf         time.Time `json:"as_of"` // datekey of the source row (UTC date)
}

// MarketCapFloat64 returns the float bridge value, exactly as Python
// float(Decimal) would (single correctly-rounded conversion).
func (f Fundamentals) MarketCapFloat64() float64 {
	return f.MarketCapUSD.Float64()
}

// Validate checks the Fundamentals invariants per the SF1 loader rules
// (spec §2.13.1: only rows with marketcap > 0 are kept).
func (f Fundamentals) Validate() error {
	if f.Ticker == "" {
		return fmt.Errorf("%w: fundamentals has empty ticker", ErrInvalidArgument)
	}
	if !f.MarketCapUSD.IsPositive() {
		return fmt.Errorf("%w: fundamentals %s has non-positive market cap %s",
			ErrInvalidArgument, f.Ticker, f.MarketCapUSD)
	}
	if f.AsOf.IsZero() {
		return fmt.Errorf("%w: fundamentals %s has zero as_of", ErrInvalidArgument, f.Ticker)
	}
	return nil
}
