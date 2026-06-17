package orb

// types.go defines the ORB-layer value types: the plain Bar at the contract
// boundary (OHLC carried as pydec to preserve decimal scale propagation), the
// target-position Signal, the per-symbol SignalIntent (UI snapshot), and the
// SignalState enum. The pure ORB layer does NOT import internal/domain (the
// signal/intent types stay free of engine types); the engine adapter translates
// between these and domain.Bar / domain.Signal.

import "time"

// SignalSide is the strategy-level direction. ORB is long-only: it emits LONG
// and FLAT only.
type SignalSide string

const (
	// SideLong maps to broker BUY.
	SideLong SignalSide = "LONG"
	// SideFlat means close the open position.
	SideFlat SignalSide = "FLAT"
	// SideShort is declared for completeness; ORB never emits it.
	SideShort SignalSide = "SHORT"
)

// Bar is the plain bar at the contract boundary. OHLC are held as
// Decimal(str(float)) and volume as int. ORB performs scale-propagating Decimal
// arithmetic on these (entry*0.99 -> 100.980), so the pure layer carries them
// as pydec — float64 would lose the rendered scale that leaks into reason /
// state_summary / state_dict.
type Bar struct {
	Symbol string
	TS     time.Time // tz-aware UTC
	Open   pydec
	High   pydec
	Low    pydec
	Close  pydec
	Volume int64
}

// NewBarFromFloats builds a Bar whose OHLC reproduce Decimal(str(f)) for each
// float, matching the runner's Decimal(str(x)) translation. Use this when
// prices originate as float64.
func NewBarFromFloats(symbol string, ts time.Time, o, h, l, c float64, vol int64) Bar {
	return Bar{
		Symbol: symbol,
		TS:     ts,
		Open:   decFromPyFloatStr(o),
		High:   decFromPyFloatStr(h),
		Low:    decFromPyFloatStr(l),
		Close:  decFromPyFloatStr(c),
		Volume: vol,
	}
}

// NewBarFromStrings builds a Bar whose OHLC parse the exact decimal literals
// (preserving their written scale), used by the golden harness which feeds the
// str(Decimal) values verbatim.
func NewBarFromStrings(symbol string, ts time.Time, o, h, l, c string, vol int64) (Bar, bool) {
	od, ok1 := parseDec(o)
	hd, ok2 := parseDec(h)
	ld, ok3 := parseDec(l)
	cd, ok4 := parseDec(c)
	if !(ok1 && ok2 && ok3 && ok4) {
		return Bar{}, false
	}
	return Bar{Symbol: symbol, TS: ts, Open: od, High: hd, Low: ld, Close: cd, Volume: vol}, true
}

// Signal is the target-position signal. For ORB, FLAT carries the *held* qty,
// and only the LONG entry sets StopPrice. StopPrice is the canonical
// str(Decimal) form, "" when nil. Confidence is always 1.0; Grade is never set
// by ORB.
type Signal struct {
	Symbol     string
	TS         time.Time
	Side       SignalSide
	TargetQty  int
	Reason     string
	Confidence float64 // always 1.0
	StopPrice  string  // str(Decimal); "" == nil
}

// SignalState is the per-symbol UI state.
type SignalState string

const (
	// StateNoSetup: range not locked, or post-EOD, or out of universe.
	StateNoSetup SignalState = "no_setup"
	// StateForming: range locked, flat, in window, close at/below orb_high.
	StateForming SignalState = "forming"
	// StateBuy: range locked, flat, in window, close > orb_high.
	StateBuy SignalState = "buy"
	// StateHold: a position is held.
	StateHold SignalState = "hold"
	// StateExit is declared for completeness (unused by the ORB intent path).
	StateExit SignalState = "exit"
	// StateStopHit is declared for completeness (unused by the ORB intent path).
	StateStopHit SignalState = "stop_hit"
)

// StrategyID is the ORB strategy id, constant "intraday_breakout".
const StrategyID = "intraday_breakout"

// SignalIntent is the typed UI snapshot.
// Optional Decimal/float fields are pointers/strings so the JSON null/absent
// distinction is preserved. ORBHigh/ORBLow encode as str(Decimal) ("" == nil).
type SignalIntent struct {
	Symbol                string
	State                 SignalState
	Strength              float64  // 0..100
	ProximityToTriggerPct *float64 // nil when not applicable
	UpdatedAt             time.Time
	Generation            int
	StrategyID            string
	ORBHigh               string     // str(Decimal); "" == nil
	ORBLow                string     // str(Decimal); "" == nil
	EntryWindowEnd        *time.Time // UTC; nil when no session yet
}
