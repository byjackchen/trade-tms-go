package orb

// types.go defines the ORB-layer value types: the plain Bar at the contract
// boundary (OHLC carried as pydec to reproduce CPython Decimal scale
// propagation byte-for-byte), the target-position Signal, the per-symbol
// SignalIntent (UI snapshot), and the SignalState enum. These mirror the frozen
// Python dataclasses in intraday_breakout/intent.py and the shared
// sepa/signal.py Bar/Signal field-for-field. The pure ORB layer does NOT import
// internal/domain (the reference keeps signal.py / intent.py free of engine
// types); the engine adapter translates between these and domain.Bar /
// domain.Signal.

import "time"

// SignalSide is the strategy-level direction (sepa/signal.py:62-67). ORB is
// long-only: it emits LONG and FLAT only.
type SignalSide string

const (
	// SideLong maps to broker BUY.
	SideLong SignalSide = "LONG"
	// SideFlat means close the open position.
	SideFlat SignalSide = "FLAT"
	// SideShort is declared for completeness; ORB never emits it.
	SideShort SignalSide = "SHORT"
)

// Bar is the plain bar at the contract boundary (sepa/signal.py:70-83). The
// reference holds OHLC as Decimal(str(float)) and volume as int. ORB performs
// scale-propagating Decimal arithmetic on these (entry*0.99 -> 100.980), so the
// pure layer must carry them as pydec — float64 would lose the rendered scale
// that leaks into reason / state_summary / state_dict.
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
// float, matching the Python test fixtures (_bar builder) and the runner's
// Decimal(str(x)) translation. Use this when prices originate as float64.
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
// (preserving their written scale), used by the parity harness which feeds the
// Python-dumped str(Decimal) values verbatim.
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

// Signal is the target-position signal (sepa/signal.py:86-101). For ORB, FLAT
// carries the *held* qty (the reference's _make_flat_signal), and only the LONG
// entry sets StopPrice. StopPrice is the canonical str(Decimal) form, "" when
// nil. Confidence is always 1.0; Grade is never set by ORB.
type Signal struct {
	Symbol     string
	TS         time.Time
	Side       SignalSide
	TargetQty  int
	Reason     string
	Confidence float64 // always 1.0
	StopPrice  string  // str(Decimal); "" == nil
}

// SignalState is the per-symbol UI state (intraday_breakout/intent.py:15-21).
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

// StrategyID is the ORB strategy id, constant "intraday_breakout"
// (intraday_breakout/intent.py:24).
const StrategyID = "intraday_breakout"

// SignalIntent is the typed UI snapshot (intraday_breakout/intent.py:27-41).
// Optional Decimal/float fields are pointers/strings so the JSON null/absent
// distinction is preserved. ORBHigh/ORBLow/ATRAtOpen encode as str(Decimal)
// ("" == nil); ATRAtOpen is reserved and always nil.
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
	ATRAtOpen             string     // always "" (reserved, never computed)
	EntryWindowEnd        *time.Time // UTC; nil when no session yet
}
