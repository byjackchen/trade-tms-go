package sepaadapter

// marshal.go converts the pure SEPA StateSummary value type into a
// JSON-serializable map whose key set and null semantics match the Python
// reference exactly: the 11-key dict (signal.py:515-539), None for empty
// optional strings. It feeds the engine's StateSummarizer publish path.
//
// NOTE: the SEPA *intent* wire shape is NOT defined here. EvaluateIntentJSON
// returns the raw sepa.SignalIntent and publish.NormalizeIntent converts it to
// the canonical domain.SEPASignalIntent — the single source of the SEPA intent
// wire shape, identical to how the other three adapters work.

import (
	"github.com/byjackchen/trade-tms-go/internal/strategy/sepa"
)

// summaryJSON is the 11-key state_summary dict (signal.py:515-539).
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
