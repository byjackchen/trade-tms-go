package orbadapter

// intent.go is the SANCTIONED ORB domain bridge (modularization-review.md §E3):
// the local→domain intent normalization relocated here from publish. The pure orb
// package emits a tag-less orb.SignalIntent (kept zero-domain for byte-for-byte
// golden output); this adapter — the only place that legitimately imports both
// orb and domain — converts it to the canonical snake_case
// domain.IntradayBreakoutSignal wire shape. publish therefore switches only on
// domain types and drops its strategy/orb import.

import (
	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/strategy/orb"
)

// NormalizeIntent converts the pure orb.SignalIntent into the canonical
// domain.IntradayBreakoutSignal — formerly publish.normalizeORB. Decimal price
// strings ("" == nil) become *domain.Price.
func NormalizeIntent(s orb.SignalIntent) domain.IntradayBreakoutSignal {
	d := domain.NewIntradayBreakoutSignal()
	d.Symbol = s.Symbol
	d.State = domain.SignalState(s.State)
	d.Strength = s.Strength
	d.ProximityToTriggerPct = s.ProximityToTriggerPct
	d.UpdatedAt = s.UpdatedAt.UTC()
	d.Generation = int64(s.Generation)
	d.ORBHigh = priceStrPtr(s.ORBHigh)
	d.ORBLow = priceStrPtr(s.ORBLow)
	if s.EntryWindowEnd != nil {
		w := s.EntryWindowEnd.UTC()
		d.EntryWindowEnd = &w
	}
	return d
}

// priceStrPtr parses a str(Decimal) price ("" == nil) into a *domain.Price. A
// non-empty value that fails to parse is dropped to nil (the "" == nil
// convention treats an unparseable price as absent rather than crashing the
// publish path).
func priceStrPtr(s string) *domain.Price {
	if s == "" {
		return nil
	}
	p, err := domain.ParsePrice(s)
	if err != nil {
		return nil
	}
	return &p
}
