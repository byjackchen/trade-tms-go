package orb

import (
	"fmt"
	"time"
)

// errInvalidStateDate wraps a malformed session-date string in LoadState.
func errInvalidStateDate(s string) error {
	return fmt.Errorf("%w: invalid current_session_date %q", ErrInvalidConfig, s)
}

// monthOf converts a 1..12 month number to time.Month.
func monthOf(m int) time.Month { return time.Month(m) }

// state.go ports state_summary (signal.py:401-424), state_dict (signal.py:430
// -460) and load_state (signal.py:462-478). These are the persistence and
// UI-monitoring surfaces; field names, ordering, null/"0" conventions and
// str(Decimal) rendering match the reference exactly.
//
// The structs use the encoding/json "omit nothing" convention with explicit
// pointers/strings for the Python None vs "0" distinctions:
//   - state_summary entry/stop/target are null when flat (pointer-to-string);
//   - state_dict entry/stop/target are ALWAYS strings (Decimal(0) -> "0" flat).

// StateSummary is the light user-visible state (signal.py:401-424). Exactly 10
// keys; JSON tags reproduce the dict key order.
type StateSummary struct {
	Symbol      string  `json:"symbol"`
	SessionDate *string `json:"session_date"` // ISO date or null
	RangeHigh   *string `json:"range_high"`   // str(Decimal) or null
	RangeLow    *string `json:"range_low"`    // str(Decimal) or null
	RangeLocked bool    `json:"range_locked"`
	AvgVolume   float64 `json:"avg_volume"`
	PositionQty int     `json:"position_qty"`
	EntryPrice  *string `json:"entry_price"`  // str(Decimal) when in position, else null
	StopPrice   *string `json:"stop_price"`   // ditto
	TargetPrice *string `json:"target_price"` // ditto
}

// StateSummary returns the UI snapshot (signal.py:401-424).
func (g *Generator) StateSummary() StateSummary {
	inPos := g.positionQty > 0
	s := StateSummary{
		Symbol:      g.cfg.Symbol,
		RangeLocked: g.rangeLocked,
		AvgVolume:   g.avgVolume,
		PositionQty: g.positionQty,
	}
	if g.currentSessionDate != nil {
		iso := g.currentSessionDate.iso()
		s.SessionDate = &iso
	}
	if g.rangeHigh != nil {
		rh := g.rangeHigh.String()
		s.RangeHigh = &rh
	}
	if g.rangeLow != nil {
		rl := g.rangeLow.String()
		s.RangeLow = &rl
	}
	if inPos {
		e := g.entryPrice.String()
		st := g.stopPrice.String()
		t := g.targetPrice.String()
		s.EntryPrice = &e
		s.StopPrice = &st
		s.TargetPrice = &t
	}
	return s
}

// StateConfig is the config sub-block of state_dict (signal.py:447-459).
// equity_at_snapshot is pulled fresh; account_size is intentionally absent.
type StateConfig struct {
	Symbol           string  `json:"symbol"`
	RiskPct          float64 `json:"risk_pct"`
	RangeMinutes     int     `json:"range_minutes"`
	VolMultiple      float64 `json:"vol_multiple"`
	ProfitTargetR    float64 `json:"profit_target_r"`
	HardStopPct      float64 `json:"hard_stop_pct"`
	EODExitTime      string  `json:"eod_exit_time"`
	Timezone         string  `json:"timezone"`
	EquityAtSnapshot float64 `json:"equity_at_snapshot"`
}

// StateDict is the full persistence snapshot (signal.py:430-460). entry/stop/
// target are always strings ("0" when flat). last_seen_close and
// intent_generation are NOT persisted (reference).
type StateDict struct {
	CurrentSessionDate *string     `json:"current_session_date"` // ISO date or null
	RangeBarsCount     int         `json:"range_bars_count"`
	RangeHigh          *string     `json:"range_high"` // str(Decimal) or null
	RangeLow           *string     `json:"range_low"`
	RangeLocked        bool        `json:"range_locked"`
	RangeTotalVolume   int64       `json:"range_total_volume"`
	AvgVolume          float64     `json:"avg_volume"`
	PositionQty        int         `json:"position_qty"`
	EntryPrice         string      `json:"entry_price"` // "0" when flat (never null)
	StopPrice          string      `json:"stop_price"`
	TargetPrice        string      `json:"target_price"`
	Config             StateConfig `json:"config"`
}

// StateDict returns the persistence snapshot (signal.py:430-460).
func (g *Generator) StateDict() StateDict {
	sd := StateDict{
		RangeBarsCount:   g.rangeBarsCount,
		RangeLocked:      g.rangeLocked,
		RangeTotalVolume: g.rangeTotalVolume,
		AvgVolume:        g.avgVolume,
		PositionQty:      g.positionQty,
		EntryPrice:       g.entryPrice.String(),
		StopPrice:        g.stopPrice.String(),
		TargetPrice:      g.targetPrice.String(),
		Config: StateConfig{
			Symbol:           g.cfg.Symbol,
			RiskPct:          g.cfg.RiskPct,
			RangeMinutes:     g.cfg.RangeMinutes,
			VolMultiple:      g.cfg.VolMultiple,
			ProfitTargetR:    g.cfg.ProfitTargetR,
			HardStopPct:      g.cfg.HardStopPct,
			EODExitTime:      g.cfg.EODExitTime,
			Timezone:         g.cfg.Timezone,
			EquityAtSnapshot: g.cfg.EquityProvider(),
		},
	}
	if g.currentSessionDate != nil {
		iso := g.currentSessionDate.iso()
		sd.CurrentSessionDate = &iso
	}
	if g.rangeHigh != nil {
		rh := g.rangeHigh.String()
		sd.RangeHigh = &rh
	}
	if g.rangeLow != nil {
		rl := g.rangeLow.String()
		sd.RangeLow = &rl
	}
	return sd
}

// LoadState restores session-level state from a StateDict (signal.py:462-478).
// config is NOT restored from the dict (the caller injects a fresh config +
// equity_provider). Missing fields fall back to the Python defaults. Returns an
// error if a Decimal string is malformed.
func (g *Generator) LoadState(sd StateDict) error {
	if sd.CurrentSessionDate != nil && *sd.CurrentSessionDate != "" {
		d, ok := parseISODate(*sd.CurrentSessionDate)
		if !ok {
			return errInvalidStateDate(*sd.CurrentSessionDate)
		}
		g.currentSessionDate = &d
	} else {
		g.currentSessionDate = nil
	}
	g.rangeBarsCount = sd.RangeBarsCount
	g.rangeHigh = parseDecPtr(sd.RangeHigh)
	g.rangeLow = parseDecPtr(sd.RangeLow)
	g.rangeLocked = sd.RangeLocked
	g.rangeTotalVolume = sd.RangeTotalVolume
	g.avgVolume = sd.AvgVolume
	g.positionQty = sd.PositionQty
	g.entryPrice = parseDecOrZero(sd.EntryPrice)
	g.stopPrice = parseDecOrZero(sd.StopPrice)
	g.targetPrice = parseDecOrZero(sd.TargetPrice)
	return nil
}

// parseDecPtr parses a *string (str(Decimal) or nil/"") into a *pydec.
// Mirrors the reference's `Decimal(rh) if rh else None` (empty/None -> None).
func parseDecPtr(s *string) *pydec {
	if s == nil || *s == "" {
		return nil
	}
	d, ok := parseDec(*s)
	if !ok {
		return nil
	}
	return &d
}

// parseDecOrZero parses a Decimal string defaulting to Decimal(0)
// (reference: Decimal(data.get("entry_price", "0"))).
func parseDecOrZero(s string) pydec {
	if s == "" {
		return decFromInt(0)
	}
	d, ok := parseDec(s)
	if !ok {
		return decFromInt(0)
	}
	return d
}

// parseISODate parses "YYYY-MM-DD" into a civilDate.
func parseISODate(s string) (civilDate, bool) {
	if len(s) != 10 || s[4] != '-' || s[7] != '-' {
		return civilDate{}, false
	}
	y, ok1 := atoiStrict(s[0:4])
	mo, ok2 := atoiStrict(s[5:7])
	d, ok3 := atoiStrict(s[8:10])
	if !ok1 || !ok2 || !ok3 || mo < 1 || mo > 12 || d < 1 || d > 31 {
		return civilDate{}, false
	}
	return civilDate{year: y, month: monthOf(mo), day: d}, true
}
