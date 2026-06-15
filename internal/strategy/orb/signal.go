package orb

// signal.go is the pure ORB (intraday_breakout) SignalGenerator, a
// byte/value-faithful port of intraday_breakout/signal.py
// (IntradayBreakoutSignalGenerator). Zero engine dependencies — bars in,
// signals out — exactly like the reference's Eng-D2 contract.
//
// Algorithm (signal.py module docstring, lines 6-20):
//  1. Build the opening range from the first range_minutes of each session:
//     range_high = max(highs), range_low = min(lows); track avg per-bar volume.
//  2. After the range locks, on each subsequent bar: if flat, check breakout
//     (close > range_high AND volume > avg*vol_multiple) -> LONG; if in a
//     position, check exits (stop / target / EOD).
//  3. Reset state at each new session; defensively flatten a lingering position
//     on a session boundary.
//
// Long-only, single-instrument, always flat by EOD. New entries are blocked on
// or after eod_exit_time.

import (
	"fmt"
	"math"
	"time"
)

// Generator is the ORB SignalGenerator. Construct with New; it owns all
// per-session mutable state (signal.py:99-112). Not safe for concurrent use.
type Generator struct {
	cfg Config
	loc *time.Location

	// Internal state (mirrors the reference field-for-field; pointers/strings
	// model the Python None where a Decimal may be absent).
	currentSessionDate *civilDate // nil == None
	rangeBarsCount     int
	rangeHigh          *pydec // nil == None
	rangeLow           *pydec // nil == None
	rangeLocked        bool
	rangeTotalVolume   int64
	avgVolume          float64
	positionQty        int
	entryPrice         pydec // Decimal(0) when flat
	stopPrice          pydec
	targetPrice        pydec
	lastSeenClose      *pydec // nil == None (for evaluate_intent)
	intentGeneration   int
}

// civilDate is a timezone-independent calendar date (the reference's
// datetime.date used for session identity). Comparison is field-wise.
type civilDate struct {
	year  int
	month time.Month
	day   int
}

func (d civilDate) equal(o civilDate) bool {
	return d.year == o.year && d.month == o.month && d.day == o.day
}

func (d civilDate) iso() string {
	return fmt.Sprintf("%04d-%02d-%02d", d.year, int(d.month), d.day)
}

// New validates cfg and returns a cold-start Generator (all the Python field
// defaults: no session, range None, flat, prices Decimal(0)).
func New(cfg Config) (*Generator, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	loc, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		return nil, fmt.Errorf("%w: timezone %q not recognized", ErrInvalidConfig, cfg.Timezone)
	}
	return &Generator{
		cfg:         cfg,
		loc:         loc,
		entryPrice:  decFromInt(0),
		stopPrice:   decFromInt(0),
		targetPrice: decFromInt(0),
	}, nil
}

// OnBar processes one bar and emits zero or more Signals, replicating
// Symbol returns the single instrument this generator trades (its universe).
func (g *Generator) Symbol() string { return g.cfg.Symbol }

// signal.py:118-171 exactly (session change handling, opening-range window,
// lock, entry/exit dispatch).
func (g *Generator) OnBar(bar Bar) []Signal {
	if bar.Symbol != g.cfg.Symbol {
		return nil // out of universe: state completely unchanged, incl last_seen_close
	}

	c := bar.Close
	g.lastSeenClose = &c

	localTS := bar.TS.In(g.loc)
	barDate := civilDate{year: localTS.Year(), month: localTS.Month(), day: localTS.Day()}

	var signals []Signal

	// Detect session change (new trading day or first-ever bar).
	if g.currentSessionDate == nil || !barDate.equal(*g.currentSessionDate) {
		// Defensive: flatten a position that somehow survived a prior session.
		if g.positionQty > 0 {
			signals = append(signals, g.makeFlatSignal(bar, "session boundary"))
		}
		g.resetSession(barDate)
	}

	// Is this bar still inside the opening-range window?
	// session_open = local_ts.replace(hour=9, minute=30, second=0, microsecond=0)
	sessionOpen := time.Date(localTS.Year(), localTS.Month(), localTS.Day(),
		sessionOpenHour, sessionOpenMinute, 0, 0, g.loc)
	rangeEnd := sessionOpen.Add(time.Duration(g.cfg.RangeMinutes) * time.Minute)

	// Strict `<`: a boundary bar whose ts == range_end is EXCLUDED from the
	// range (signal.py:146-155, the conservative call).
	if localTS.Before(rangeEnd) {
		g.extendRange(bar)
		return signals
	}

	// Range has elapsed. Lock it (idempotent) before any entry/exit logic.
	if !g.rangeLocked {
		g.lockRange()
	}
	if !g.rangeLocked {
		// Still couldn't lock (no bars accumulated — joined mid-session).
		// Skip entry logic for this session.
		return signals
	}

	if g.positionQty > 0 {
		signals = append(signals, g.maybeExit(bar, localTS)...)
	} else {
		signals = append(signals, g.maybeEnter(bar, localTS)...)
	}
	return signals
}

// extendRange accumulates one bar into the opening range (signal.py:177-183):
// range_high = max(highs), range_low = min(lows), cumulative volume + count.
func (g *Generator) extendRange(bar Bar) {
	if g.rangeHigh == nil || bar.High.cmp(*g.rangeHigh) > 0 {
		h := bar.High
		g.rangeHigh = &h
	}
	if g.rangeLow == nil || bar.Low.cmp(*g.rangeLow) < 0 {
		l := bar.Low
		g.rangeLow = &l
	}
	g.rangeTotalVolume += bar.Volume
	g.rangeBarsCount++
}

// lockRange computes avg_volume and sets locked, unless no bars accumulated
// (signal.py:185-190). Idempotent on the locked path (caller guards).
func (g *Generator) lockRange() {
	if g.rangeBarsCount == 0 || g.rangeHigh == nil {
		return
	}
	g.avgVolume = float64(g.rangeTotalVolume) / float64(g.rangeBarsCount)
	g.rangeLocked = true
}

// maybeEnter applies the breakout entry logic (signal.py:196-248).
func (g *Generator) maybeEnter(bar Bar, localTS time.Time) []Signal {
	// Block new entries on or after EOD.
	if !localTS.Before(g.eodDT(localTS)) {
		return nil
	}
	if g.rangeHigh == nil || g.rangeLow == nil {
		return nil
	}

	// Breakout check: close > range_high AND volume > avg * vol_multiple
	// (both strict; equality does not trigger).
	if bar.Close.cmp(*g.rangeHigh) <= 0 {
		return nil
	}
	if float64(bar.Volume) <= g.avgVolume*g.cfg.VolMultiple {
		return nil
	}

	entry := bar.Close
	// hard_stop = entry * (1 - Decimal(str(hard_stop_pct))/100)  [all Decimal]
	hardStop := entry.mul(decFromInt(1).sub(decFromPyFloatStr(g.cfg.HardStopPct).divInt(100)))
	// stop = max(range_low, hard_stop) — picks the TIGHTER stop (closer to entry).
	stop := *g.rangeLow
	if hardStop.cmp(stop) > 0 {
		stop = hardStop
	}
	if stop.cmp(entry) >= 0 {
		return nil // degenerate: non-positive stop distance
	}

	stopDistance := entry.sub(stop)
	target := entry.add(stopDistance.mul(decFromPyFloatStr(g.cfg.ProfitTargetR)))

	equity := g.cfg.EquityProvider()
	riskDollar := equity * (g.cfg.RiskPct / 100)
	// shares = int(risk_dollar // float(stop_distance)) — float floor-div, trunc.
	shares := int(math.Floor(riskDollar / stopDistance.float64()))
	if shares < 1 {
		return nil
	}

	// Persist state.
	g.positionQty = shares
	g.entryPrice = entry
	g.stopPrice = stop
	g.targetPrice = target

	reason := fmt.Sprintf(
		"ORB breakout: close %s > range_high %s, vol %d > avg %s * %s :: stop %s, target %s",
		entry.String(), g.rangeHigh.String(), bar.Volume, pyFmt0(g.avgVolume),
		pyFloatRepr(g.cfg.VolMultiple), stop.String(), target.String(),
	)
	return []Signal{{
		Symbol:     g.cfg.Symbol,
		TS:         bar.TS,
		Side:       SideLong,
		TargetQty:  shares,
		Reason:     reason,
		Confidence: 1.0,
		StopPrice:  g.stopPrice.String(),
	}}
}

// maybeExit applies the exit logic with strict priority EOD > stop > target,
// at most one signal per bar (signal.py:254-264).
func (g *Generator) maybeExit(bar Bar, localTS time.Time) []Signal {
	if !localTS.Before(g.eodDT(localTS)) {
		return []Signal{g.makeFlatSignal(bar, "EOD exit at "+g.cfg.EODExitTime)}
	}
	if bar.Low.cmp(g.stopPrice) <= 0 {
		return []Signal{g.makeFlatSignal(bar, "stop hit at "+g.stopPrice.String())}
	}
	if bar.High.cmp(g.targetPrice) >= 0 {
		return []Signal{g.makeFlatSignal(bar, "target hit at "+g.targetPrice.String())}
	}
	return nil
}

// makeFlatSignal emits a FLAT carrying the pre-close held qty and then clears
// position state (signal.py:266-278).
func (g *Generator) makeFlatSignal(bar Bar, reason string) Signal {
	sig := Signal{
		Symbol:     g.cfg.Symbol,
		TS:         bar.TS,
		Side:       SideFlat,
		TargetQty:  g.positionQty,
		Reason:     reason,
		Confidence: 1.0,
	}
	g.positionQty = 0
	g.entryPrice = decFromInt(0)
	g.stopPrice = decFromInt(0)
	g.targetPrice = decFromInt(0)
	return sig
}

// eodDT returns the EOD-exit instant on localTS's calendar day in the config
// timezone (signal.py:284-288: local_ts.replace(hour, minute, 0, 0)).
func (g *Generator) eodDT(localTS time.Time) time.Time {
	h, m, _ := parseHHMM(g.cfg.EODExitTime)
	return time.Date(localTS.Year(), localTS.Month(), localTS.Day(), h, m, 0, 0, g.loc)
}

// resetSession clears all per-session state for new_date (signal.py:290-301).
func (g *Generator) resetSession(newDate civilDate) {
	d := newDate
	g.currentSessionDate = &d
	g.rangeBarsCount = 0
	g.rangeHigh = nil
	g.rangeLow = nil
	g.rangeLocked = false
	g.rangeTotalVolume = 0
	g.avgVolume = 0.0
	g.positionQty = 0
	g.entryPrice = decFromInt(0)
	g.stopPrice = decFromInt(0)
	g.targetPrice = decFromInt(0)
}
