package domain

// intent.go defines the SignalIntent family: a shared core (the 6 fields +
// strategy_id common to all four strategies) plus the four strategy-specific
// payloads, mirroring the frozen Python dataclasses (spec §2.6 [MUST-MATCH]).
//
// JSON field names and declaration order replicate Python
// dataclasses.asdict, which preserves declaration order.

import (
	"math"
	"time"
)

// Logical strategy IDs used INSIDE SignalIntent payloads [MUST-MATCH].
// These are distinct from the engine-level strategy ids (e.g.
// "SEPARunner-000") used for orders, positions and allocator keys (§7.7);
// the two id spaces must never be conflated.
const (
	StrategyIDSEPA             = "sepa"
	StrategyIDPairs            = "pairs"
	StrategyIDSectorRotation   = "sector_rotation"
	StrategyIDIntradayBreakout = "intraday_breakout"
)

// SignalIntent is the shared head of every strategy intent (spec §2.6):
// the UI-facing "what is the strategy thinking" snapshot.
//
//   - Strength is 0..100.
//   - ProximityToTriggerPct is nil when not applicable.
//   - UpdatedAt is timezone-aware UTC.
//   - Generation is a per-generator monotonically increasing counter,
//     incremented on every evaluate_intent call and intentionally NOT
//     persisted (restarts reset it to 0).
type SignalIntent struct {
	Symbol                string      `json:"symbol"`
	State                 SignalState `json:"state"`
	Strength              float64     `json:"strength"`
	ProximityToTriggerPct *float64    `json:"proximity_to_trigger_pct"`
	UpdatedAt             time.Time   `json:"updated_at"`
	Generation            int64       `json:"generation"`
	StrategyID            string      `json:"strategy_id"`
}

// SEPASignalIntent — src/strategies/sepa/intent.py:30-55 [MUST-MATCH].
// Grade here is the integer trend-template grade 0..100 (not the letter
// Grade). PivotPrice/StopPrice are included only when > 0, else nil.
// RSRank is reserved and never set by the reference.
type SEPASignalIntent struct {
	SignalIntent
	Grade             int      `json:"grade"`
	TrendTemplatePass bool     `json:"trend_template_pass"`
	BaseAgeDays       *int     `json:"base_age_days"`
	BaseDepthPct      *float64 `json:"base_depth_pct"`
	VolumeDryup       *bool    `json:"volume_dryup"`
	PivotPrice        *Price   `json:"pivot_price"`
	StopPrice         *Price   `json:"stop_price"`
	RSRank            *int     `json:"rs_rank"`
}

// NewSEPASignalIntent returns a SEPASignalIntent with the Python defaults:
// strategy_id "sepa", grade 0, all optionals nil.
func NewSEPASignalIntent() SEPASignalIntent {
	return SEPASignalIntent{SignalIntent: SignalIntent{StrategyID: StrategyIDSEPA}}
}

// PairsSignalIntent — src/strategies/pairs/intent.py:28-44 [MUST-MATCH].
// PairID has the format "{long_leg}/{short_leg}". HalfLifeDays is reserved
// and always 0.0 in the reference.
type PairsSignalIntent struct {
	SignalIntent
	PairID          string  `json:"pair_id"`
	LegRole         LegRole `json:"leg_role"`
	ZScore          float64 `json:"z_score"`
	ZEntryThreshold float64 `json:"z_entry_threshold"`
	ZExitThreshold  float64 `json:"z_exit_threshold"`
	HedgeRatio      float64 `json:"hedge_ratio"`
	HalfLifeDays    float64 `json:"half_life_days"`
}

// NewPairsSignalIntent returns a PairsSignalIntent with the Python defaults:
// strategy_id "pairs", leg_role "long", z_entry 2.0, z_exit 0.5, hedge 1.0.
func NewPairsSignalIntent() PairsSignalIntent {
	return PairsSignalIntent{
		SignalIntent:    SignalIntent{StrategyID: StrategyIDPairs},
		LegRole:         LegLong,
		ZEntryThreshold: 2.0,
		ZExitThreshold:  0.5,
		HedgeRatio:      1.0,
	}
}

// StrengthFromZ maps |z| to a 0..100 strength
// (src/strategies/pairs/intent.py:47-49 [MUST-MATCH]):
// min(100.0, abs(z)/3.0*100.0).
func StrengthFromZ(zAbs float64) float64 {
	return math.Min(100.0, math.Abs(zAbs)/3.0*100.0)
}

// SectorRotationIntent — src/strategies/sector_rotation/intent.py:27-40
// [MUST-MATCH]. Rank is 1-based (1 = best momentum); 0 = unranked/warming up.
type SectorRotationIntent struct {
	SignalIntent
	MomentumScore float64 `json:"momentum_score"`
	Rank          int     `json:"rank"`
	TargetWeight  float64 `json:"target_weight"`
	CurrentWeight float64 `json:"current_weight"`
}

// NewSectorRotationIntent returns a SectorRotationIntent with the Python
// defaults: strategy_id "sector_rotation", all numerics 0.
func NewSectorRotationIntent() SectorRotationIntent {
	return SectorRotationIntent{SignalIntent: SignalIntent{StrategyID: StrategyIDSectorRotation}}
}

// StrengthFromRank maps a 1-based momentum rank to 0..100 strength
// (src/strategies/sector_rotation/intent.py:43-49 [MUST-MATCH]):
//
//	total <= 1 or rank <= 1 → 100.0 if rank == 1 else 0.0
//	rank >= total           → 0.0
//	else                    → max(0.0, 100.0 - (rank-1)/(total-1)*100.0)
func StrengthFromRank(rank, total int) float64 {
	if total <= 1 || rank <= 1 {
		if rank == 1 {
			return 100.0
		}
		return 0.0
	}
	if rank >= total {
		return 0.0
	}
	// The explicit float64(...) conversion around the product is REQUIRED:
	// it forces the multiplication to round before the subtraction, which
	// prevents Go from contracting "100.0 - x*100.0" into a fused
	// multiply-add on arm64. CPython never fuses, so without the conversion
	// this expression diverges from the reference in the last bit
	// (e.g. rank=10, total=11: FMA yields 9.999999999999998, CPython 10.0).
	return math.Max(0.0, 100.0-float64(float64(rank-1)/float64(total-1)*100.0))
}

// IntradayBreakoutIntent — src/strategies/intraday_breakout/intent.py:27-41
// [MUST-MATCH]. ATRAtOpen is reserved and always nil in the reference.
// EntryWindowEnd is the session's EOD-exit instant converted to UTC.
type IntradayBreakoutIntent struct {
	SignalIntent
	ORBHigh        *Price     `json:"orb_high"`
	ORBLow         *Price     `json:"orb_low"`
	ATRAtOpen      *Price     `json:"atr_at_open"`
	EntryWindowEnd *time.Time `json:"entry_window_end"`
}

// NewIntradayBreakoutIntent returns an IntradayBreakoutIntent with the
// Python defaults: strategy_id "intraday_breakout", all optionals nil.
func NewIntradayBreakoutIntent() IntradayBreakoutIntent {
	return IntradayBreakoutIntent{SignalIntent: SignalIntent{StrategyID: StrategyIDIntradayBreakout}}
}
