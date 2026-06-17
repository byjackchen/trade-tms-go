package sepaadapter

// intent.go is the SANCTIONED SEPA domain bridge (modularization-review.md §E3):
// the local→domain signal normalization relocated here from publish. The pure
// sepa package emits a tag-less sepa.SignalSnapshot (kept zero-domain for
// byte-for-byte golden output, sepa/doc.go §11-14); this adapter — the only place
// that legitimately imports both sepa and domain — converts it to the canonical
// snake_case domain.SEPASignal wire shape. publish therefore switches only
// on domain types and drops its strategy/sepa import.

import (
	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/strategy/sepa"
)

// NormalizeSignal converts the pure sepa.SignalSnapshot (no json tags) into the
// canonical domain.SEPASignal. It is the single source of the SEPA signal
// wire shape — formerly publish.normalizeSEPA.
// Decimal price strings ("" == nil) become *domain.Price.
func NormalizeSignal(s sepa.SignalSnapshot) domain.SEPASignal {
	d := domain.NewSEPASignal()
	d.Symbol = s.Symbol
	d.State = domain.SignalState(s.State)
	d.Strength = s.Strength
	d.ProximityToTriggerPct = s.ProximityToTriggerP
	d.UpdatedAt = s.UpdatedAt.UTC()
	d.Generation = int64(s.Generation)
	d.Grade = s.Grade
	d.TrendTemplatePass = s.TrendTemplatePass
	d.BaseAgeDays = s.BaseAgeDays
	d.BaseDepthPct = s.BaseDepthPct
	d.VolumeDryup = s.VolumeDryup
	d.PivotPrice = priceStrPtr(s.PivotPrice)
	d.StopPrice = priceStrPtr(s.StopPrice)
	d.RSRank = s.RSRank
	// TMS ENHANCEMENT passthrough: the actionable trade-plan float fields.
	d.RiskPct = s.RiskPct
	d.PctOff52wkHigh = s.PctOff52wkH
	d.VolRatio = s.VolRatio
	d.BuyReadiness = s.BuyReadiness
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
