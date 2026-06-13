package sepa

// types.go defines the SEPA-layer value types: the plain Bar at the contract
// boundary, the target-position Signal, the per-symbol SignalIntent (UI
// snapshot), and the SignalState / Grade enums. These mirror the frozen Python
// dataclasses in sepa/signal.py and sepa/intent.py field-for-field. The pure
// SEPA layer does NOT import internal/domain (the reference keeps signal.py /
// intent.py free of engine types); the engine adapter translates between these
// and domain.Bar / domain.Signal.

import (
	"time"
)

// SignalSide is the strategy-level direction (sepa/signal.py:62-67). SHORT is
// declared but unused (long-only SEPA).
type SignalSide string

const (
	// SideLong maps to broker BUY.
	SideLong SignalSide = "LONG"
	// SideFlat means close-everything.
	SideFlat SignalSide = "FLAT"
	// SideShort is declared for completeness; SEPA never emits it.
	SideShort SignalSide = "SHORT"
)

// Grade is the SEPA setup grade (sepa/grade.py:13): "A+", "B", or "skip".
type Grade string

const (
	// GradeAPlus is the highest grade (3 tranches).
	GradeAPlus Grade = "A+"
	// GradeB is the standard grade (2 tranches).
	GradeB Grade = "B"
	// GradeSkip means no entry.
	GradeSkip Grade = "skip"
)

// Bar is the plain-Python bar at the contract boundary (sepa/signal.py:70-83).
// The reference holds OHLC as Decimal and volume as int; the internal kline
// buffer stores them float64/int (signal.py:360-369). We carry OHLC as float64
// directly: every downstream computation (and the stored entry-price string)
// derives from float(bar.X) / Decimal(str(close)), and the reference's bar
// closes are always Decimal(str(float)), so float64 here is lossless and the
// entry-price string is reproduced via pyFloatRepr(close).
type Bar struct {
	Symbol string
	TS     time.Time // tz-aware UTC
	Open   float64
	High   float64
	Low    float64
	Close  float64
	Volume int64
}

// Signal is the target-position signal (sepa/signal.py:86-101). TargetQty is
// signed (positive long, 0 flat). Grade is "" when unset; StopPrice is the
// canonical Python str(Decimal) form ("" when nil).
type Signal struct {
	Symbol     string
	TS         time.Time
	Side       SignalSide
	TargetQty  int
	Reason     string
	Confidence float64 // always 1.0
	Grade      Grade   // "" == nil
	StopPrice  string  // str(Decimal); "" == nil
}

// SignalState is the per-symbol UI state (sepa/intent.py SignalState).
type SignalState string

const (
	// StateNoSetup means no actionable setup.
	StateNoSetup SignalState = "no_setup"
	// StateForming means a base is forming below pivot.
	StateForming SignalState = "forming"
	// StateBuy means price is at/above pivot (only reachable with a primed pivot).
	StateBuy SignalState = "buy"
	// StateHold means a position is held normally.
	StateHold SignalState = "hold"
	// StateExit is declared for completeness (unused by the reference path).
	StateExit SignalState = "exit"
	// StateStopHit means a held position is below its stop.
	StateStopHit SignalState = "stop_hit"
)

// StrategyID is the SEPA strategy id, constant "sepa" (intent.py:27).
const StrategyID = "sepa"

// SignalIntent is the typed UI snapshot (sepa/intent.py:30-56). Optional
// numeric/Decimal fields are pointers so the JSON null/absent distinction is
// preserved (Decimal fields encode as str(Decimal)).
type SignalIntent struct {
	Symbol              string
	State               SignalState
	Strength            float64 // 0..100
	ProximityToTriggerP *float64
	UpdatedAt           time.Time
	Generation          int
	StrategyID          string
	Grade               int // 0..100 (== passing_rules/8*100, int-truncated)
	TrendTemplatePass   bool
	BaseAgeDays         *int
	BaseDepthPct        *float64
	VolumeDryup         *bool
	PivotPrice          string // str(Decimal); "" == nil
	StopPrice           string // str(Decimal); "" == nil
	RSRank              *int   // always nil
}
