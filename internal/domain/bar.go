package domain

// bar.go defines the project-wide OHLCV bar contract (spec §2.2), reused by
// all four strategies.

import (
	"fmt"
	"time"
)

// Bar is one OHLCV bar. Immutable by convention: pass by value, never mutate
// a received Bar.
//
// Invariants (spec §2.2):
//   - Symbol is the plain ticker (e.g. "AAPL", no venue suffix), taken from
//     the bar type's instrument id at translation time.
//   - TS is timezone-aware UTC.
//   - OHLC prices come from the data layer 2-decimal exact
//     (price_precision=2) and are held exactly by the 1e-4 fixed point.
//   - Volume is a truncating cast from the source quantity.
type Bar struct {
	Symbol string    `json:"symbol"`
	TS     time.Time `json:"ts"`
	Open   Price     `json:"open"`
	High   Price     `json:"high"`
	Low    Price     `json:"low"`
	Close  Price     `json:"close"`
	Volume int64     `json:"volume"`
}

// Validate checks the Bar invariants. It is opt-in (bars are not validated on
// construction); call it at ingestion boundaries.
func (b Bar) Validate() error {
	if b.Symbol == "" {
		return fmt.Errorf("%w: bar has empty symbol", ErrInvalidArgument)
	}
	if b.TS.IsZero() {
		return fmt.Errorf("%w: bar %s has zero timestamp", ErrInvalidArgument, b.Symbol)
	}
	if _, off := b.TS.Zone(); off != 0 {
		return fmt.Errorf("%w: bar %s timestamp %s is not UTC", ErrInvalidArgument, b.Symbol, b.TS)
	}
	if b.High < b.Low {
		return fmt.Errorf("%w: bar %s@%s has high %s < low %s",
			ErrInvalidArgument, b.Symbol, b.TS.Format(time.RFC3339), b.High, b.Low)
	}
	if b.Open < b.Low || b.Open > b.High {
		return fmt.Errorf("%w: bar %s@%s open %s outside [low %s, high %s]",
			ErrInvalidArgument, b.Symbol, b.TS.Format(time.RFC3339), b.Open, b.Low, b.High)
	}
	if b.Close < b.Low || b.Close > b.High {
		return fmt.Errorf("%w: bar %s@%s close %s outside [low %s, high %s]",
			ErrInvalidArgument, b.Symbol, b.TS.Format(time.RFC3339), b.Close, b.Low, b.High)
	}
	if b.Volume < 0 {
		return fmt.Errorf("%w: bar %s@%s has negative volume %d",
			ErrInvalidArgument, b.Symbol, b.TS.Format(time.RFC3339), b.Volume)
	}
	return nil
}
