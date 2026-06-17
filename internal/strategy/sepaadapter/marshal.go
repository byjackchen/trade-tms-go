package sepaadapter

// marshal.go converts the pure SEPA StateSummary value type into a
// JSON-serializable map with a fixed key set and null semantics: the 11-key
// dict, null for empty optional strings. It feeds the engine's StateSummarizer
// publish path.
//
// NOTE: the SEPA *signal* wire shape is NOT defined here. EvaluateSignalJSON
// returns the raw sepa.SignalSnapshot and sepaadapter.NormalizeSignal converts it to
// the canonical domain.SEPASignal — the single source of the SEPA signal
// wire shape, identical to how the other three adapters work.

import (
	"github.com/byjackchen/trade-tms-go/internal/strategy/sepa"
)

// summaryJSON is the 11-key state_summary dict.
type summaryJSON struct {
	Symbol        string  `json:"symbol"`
	Regime        string  `json:"regime"`
	MarketCapUSD  float64 `json:"market_cap_usd"`
	InBlackout    bool    `json:"in_blackout"`
	PositionQty   int     `json:"position_qty"`
	EntryPrice    *string `json:"entry_price"`
	StopPrice     *string `json:"stop_price"`
	CurrentGrade  *string `json:"current_grade"`
	VCPDetected   bool    `json:"vcp_detected"`
	PivotPrice    *string `json:"pivot_price"`
	BarsInHistory int     `json:"bars_in_history"`
}

func marshalSummary(s sepa.StateSummary) summaryJSON {
	return summaryJSON{
		Symbol:        s.Symbol,
		Regime:        s.Regime,
		MarketCapUSD:  s.MarketCapUSD,
		InBlackout:    s.InBlackout,
		PositionQty:   s.PositionQty,
		EntryPrice:    emptyToNil(s.EntryPrice),
		StopPrice:     emptyToNil(s.StopPrice),
		CurrentGrade:  emptyToNil(s.CurrentGrade),
		VCPDetected:   s.VCPDetected,
		PivotPrice:    emptyToNil(s.PivotPrice),
		BarsInHistory: s.BarsInHistory,
	}
}

func emptyToNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
