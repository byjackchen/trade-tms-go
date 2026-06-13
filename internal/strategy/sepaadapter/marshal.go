package sepaadapter

// marshal.go converts the pure SEPA value types into JSON-serializable maps
// whose key set and null semantics match the Python reference exactly:
//   - SignalIntent  -> dataclasses.asdict(SEPASignalIntent) with Decimal/datetime
//     str-encoded (intent.py JSON round-trip: asdict + default=str).
//   - StateSummary  -> the 11-key dict (signal.py:515-539), None for empty
//     optional strings.
// These feed the engine's IntentEvaluator / StateSummarizer publish paths.

import (
	"time"

	"github.com/byjackchen/trade-tms-go/internal/strategy/sepa"
)

// intentJSON mirrors the field set of sepa/intent.py SEPASignalIntent. Pointer
// fields are omitted-as-null when nil; Decimal fields are str-encoded.
type intentJSON struct {
	Symbol              string   `json:"symbol"`
	State               string   `json:"state"`
	Strength            float64  `json:"strength"`
	ProximityToTriggerP *float64 `json:"proximity_to_trigger_pct"`
	UpdatedAt           string   `json:"updated_at"`
	Generation          int      `json:"generation"`
	StrategyID          string   `json:"strategy_id"`
	Grade               int      `json:"grade"`
	TrendTemplatePass   bool     `json:"trend_template_pass"`
	BaseAgeDays         *int     `json:"base_age_days"`
	BaseDepthPct        *float64 `json:"base_depth_pct"`
	VolumeDryup         *bool    `json:"volume_dryup"`
	PivotPrice          *string  `json:"pivot_price"`
	StopPrice           *string  `json:"stop_price"`
	RSRank              *int     `json:"rs_rank"`
}

func marshalIntent(it sepa.SignalIntent) intentJSON {
	return intentJSON{
		Symbol:              it.Symbol,
		State:               string(it.State),
		Strength:            it.Strength,
		ProximityToTriggerP: it.ProximityToTriggerP,
		UpdatedAt:           it.UpdatedAt.UTC().Format(time.RFC3339Nano),
		Generation:          it.Generation,
		StrategyID:          it.StrategyID,
		Grade:               it.Grade,
		TrendTemplatePass:   it.TrendTemplatePass,
		BaseAgeDays:         it.BaseAgeDays,
		BaseDepthPct:        it.BaseDepthPct,
		VolumeDryup:         it.VolumeDryup,
		PivotPrice:          emptyToNil(it.PivotPrice),
		StopPrice:           emptyToNil(it.StopPrice),
		RSRank:              it.RSRank,
	}
}

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
