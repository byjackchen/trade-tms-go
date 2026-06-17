package domain

// intent.go defines the Signal family: a shared core (the 6 fields +
// strategy_id common to all four strategies) plus the four strategy-specific
// payloads (spec §2.6).
//
// JSON field names follow declaration order, which the encoder preserves.

import (
	"math"
	"time"
)

// Logical strategy IDs used INSIDE Signal payloads.
// These are distinct from the engine-level strategy ids (e.g.
// "SEPARunner-000") used for orders, positions and allocator keys (§7.7);
// the two id spaces must never be conflated.
const (
	StrategyIDSEPA             = "sepa"
	StrategyIDPairs            = "pairs"
	StrategyIDSectorRotation   = "sector_rotation"
	StrategyIDIntradayBreakout = "intraday_breakout"
)

// SignalCore is the shared head of every strategy signal (spec §2.6):
// the UI-facing "what is the strategy thinking" snapshot. It is embedded by
// each strategy-specific *Signal payload below. (It is NOT the target-position
// domain.Signal in signal.go — that is a distinct, executable target/qty type.)
//
//   - Strength is 0..100.
//   - ProximityToTriggerPct is nil when not applicable.
//   - UpdatedAt is timezone-aware UTC.
//   - Generation is a per-generator monotonically increasing counter,
//     incremented on every evaluate_intent call and intentionally NOT
//     persisted (restarts reset it to 0).
type SignalCore struct {
	Symbol                string      `json:"symbol"`
	State                 SignalState `json:"state"`
	Strength              float64     `json:"strength"`
	ProximityToTriggerPct *float64    `json:"proximity_to_trigger_pct"`
	UpdatedAt             time.Time   `json:"updated_at"`
	Generation            int64       `json:"generation"`
	StrategyID            string      `json:"strategy_id"`
}

// SEPASignal is the SEPA strategy intent payload.
// Grade here is the integer trend-template grade 0..100 (not the letter
// Grade). PivotPrice/StopPrice are included only when > 0, else nil.
// RSRank is nil at generation time and stamped cross-sectionally by the EOD
// refresh (runner/rs_rank.go).
type SEPASignal struct {
	SignalCore
	Grade             int      `json:"grade"`
	TrendTemplatePass bool     `json:"trend_template_pass"`
	BaseAgeDays       *int     `json:"base_age_days"`
	BaseDepthPct      *float64 `json:"base_depth_pct"`
	VolumeDryup       *bool    `json:"volume_dryup"`
	PivotPrice        *Price   `json:"pivot_price"`
	StopPrice         *Price   `json:"stop_price"`
	RSRank            *int     `json:"rs_rank"`

	// --- Actionable trade-plan fields ----------------------------------------
	// Persisted in the signals.signal JSONB.
	// For state=forming these are always non-null (see strategy/sepa intent.go).
	RiskPct        *float64 `json:"risk_pct"`
	PctOff52wkHigh *float64 `json:"pct_off_52wk_high"`
	VolRatio       *float64 `json:"vol_ratio"`
	BuyReadiness   *float64 `json:"buy_readiness"`
}

// NewSEPASignal returns a SEPASignal with the defaults:
// strategy_id "sepa", grade 0, all optionals nil.
func NewSEPASignal() SEPASignal {
	return SEPASignal{SignalCore: SignalCore{StrategyID: StrategyIDSEPA}}
}

// PairsSignal is the pairs strategy intent payload.
// PairID has the format "{long_leg}/{short_leg}".
type PairsSignal struct {
	SignalCore
	PairID          string  `json:"pair_id"`
	LegRole         LegRole `json:"leg_role"`
	ZScore          float64 `json:"z_score"`
	ZEntryThreshold float64 `json:"z_entry_threshold"`
	ZExitThreshold  float64 `json:"z_exit_threshold"`
	HedgeRatio      float64 `json:"hedge_ratio"`
}

// NewPairsSignal returns a PairsSignal with the defaults:
// strategy_id "pairs", leg_role "long", z_entry 2.0, z_exit 0.5, hedge 1.0.
func NewPairsSignal() PairsSignal {
	return PairsSignal{
		SignalCore:      SignalCore{StrategyID: StrategyIDPairs},
		LegRole:         LegLong,
		ZEntryThreshold: 2.0,
		ZExitThreshold:  0.5,
		HedgeRatio:      1.0,
	}
}

// StrengthFromZ maps |z| to a 0..100 strength (spec §2.6):
// min(100.0, abs(z)/3.0*100.0).
func StrengthFromZ(zAbs float64) float64 {
	return math.Min(100.0, math.Abs(zAbs)/3.0*100.0)
}

// SectorRotationSignal is the sector-rotation strategy intent payload.
// Rank is 1-based (1 = best momentum); 0 = unranked/warming up.
type SectorRotationSignal struct {
	SignalCore
	MomentumScore float64 `json:"momentum_score"`
	Rank          int     `json:"rank"`
	TargetWeight  float64 `json:"target_weight"`
	CurrentWeight float64 `json:"current_weight"`
}

// NewSectorRotationSignal returns a SectorRotationSignal with the defaults:
// strategy_id "sector_rotation", all numerics 0.
func NewSectorRotationSignal() SectorRotationSignal {
	return SectorRotationSignal{SignalCore: SignalCore{StrategyID: StrategyIDSectorRotation}}
}

// StrengthFromRank maps a 1-based momentum rank to 0..100 strength (spec §2.6):
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
	// The explicit float64(...) conversion around the product is REQUIRED for
	// cross-platform numeric determinism: it forces the multiplication to
	// round before the subtraction, which prevents the compiler from
	// contracting "100.0 - x*100.0" into a fused multiply-add (FMA) on arm64.
	// Without it the result differs by one bit between arm64 (which fuses) and
	// x86 (which does not) — e.g. rank=10, total=11: the fused form yields
	// 9.999999999999998, the rounded form 10.0. Suppressing the fusion keeps
	// the backtest/hyperopt output bit-identical across platforms.
	return math.Max(0.0, 100.0-float64(float64(rank-1)/float64(total-1)*100.0))
}

// IntradayBreakoutSignal is the intraday-breakout strategy intent payload.
// EntryWindowEnd is the session's EOD-exit instant converted to UTC.
type IntradayBreakoutSignal struct {
	SignalCore
	ORBHigh        *Price     `json:"orb_high"`
	ORBLow         *Price     `json:"orb_low"`
	EntryWindowEnd *time.Time `json:"entry_window_end"`
}

// NewIntradayBreakoutSignal returns an IntradayBreakoutSignal with the
// defaults: strategy_id "intraday_breakout", all optionals nil.
func NewIntradayBreakoutSignal() IntradayBreakoutSignal {
	return IntradayBreakoutSignal{SignalCore: SignalCore{StrategyID: StrategyIDIntradayBreakout}}
}
