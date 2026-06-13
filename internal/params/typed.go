package params

// typed.go defines the per-strategy resolved parameter structs each Go strategy
// consumes, plus the runtime validation that mirrors the Python
// *SignalGeneratorConfig.__post_init__ bodies EXACTLY (same predicates, same
// error messages). These are the [MUST-MATCH] config-validation rules:
//
//   sepa     (signal.py:155-163): risk_pct in (0,100]; hard_stop_pct > 0.
//   pairs    (signal.py:92-104):  pairs non-empty; lookback >= 5;
//                                 entry_z > 0 and exit_z >= 0;
//                                 exit_z < entry_z; capital_per_pair_pct in (0,1].
//   sector   (signal.py:80-92):   universe non-empty; momentum_lookback >= 2;
//                                 top_k in [1, len(universe)].
//   intraday (signal.py:65-91):   risk_pct in (0,100]; range_minutes >= 1;
//                                 vol_multiple > 0; profit_target_r > 0;
//                                 hard_stop_pct in (0,50]; eod_exit_time HH:MM
//                                 with 0<=h<=23, 0<=m<=59; timezone IANA-loadable.
//
// equity_provider validation is intentionally NOT modeled here: it is an
// injected runtime closure (Python validates only callability), supplied by the
// engine at construction, not a declared parameter.
//
// Numbers are decoded as float64/int64 mirroring Python float/int. A param
// declared "int" whose JSON default is a whole number decodes cleanly; a
// non-integral value for an int param is a load error (Python would have an int
// there by construction).

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Pair is one pairs-trading leg pair (pairs/signal.py Pair). long_leg /
// short_leg labels are arbitrary; the strategy trades the spread both ways.
type Pair struct {
	LongLeg  string
	ShortLeg string
}

// SEPAParams is the resolved SEPA configuration (sepa/signal.py
// SEPASignalGeneratorConfig minus symbol/equity_provider, which the engine
// injects per-instrument at construction).
type SEPAParams struct {
	RiskPct                float64
	MarketCapMinUSD        float64
	HardStopPct            float64
	PivotBufferPct         float64
	BreakoutVolumeMultiple float64
	VCPLookback            int64
	HistoryMaxBars         int64
	Timezone               string
}

// PairsParams is the resolved Pairs configuration (pairs/signal.py
// PairsSignalGeneratorConfig minus equity_provider).
type PairsParams struct {
	Pairs             []Pair
	Lookback          int64
	EntryZ            float64
	ExitZ             float64
	CapitalPerPairPct float64
	Timezone          string
}

// SectorRotationParams is the resolved Sector Rotation configuration
// (sector_rotation/signal.py minus equity_provider).
type SectorRotationParams struct {
	Universe         []string
	MomentumLookback int64
	TopK             int64
	Timezone         string
}

// IntradayBreakoutParams is the resolved Intraday ORB configuration
// (intraday_breakout/signal.py minus symbol/equity_provider).
type IntradayBreakoutParams struct {
	RiskPct       float64
	RangeMinutes  int64
	VolMultiple   float64
	ProfitTargetR float64
	HardStopPct   float64
	EODExitTime   string
	Timezone      string
}

// ---------------------------------------------------------------------------
// decoding helpers — pull typed values out of a Defaults()/resolved param map.
// ---------------------------------------------------------------------------

// pmap is a resolved param map (name -> Go value, numbers as float64).
type pmap map[string]any

func (m pmap) float(name string) (float64, error) {
	v, ok := m[name]
	if !ok {
		return 0, fmt.Errorf("missing parameter %q", name)
	}
	f, ok := v.(float64)
	if !ok {
		return 0, fmt.Errorf("parameter %q: expected number, got %T", name, v)
	}
	return f, nil
}

// integer pulls a parameter that must be an exact integer. JSON numbers decode
// to float64; a non-integral value is rejected (Python's int() over a declared
// int param never silently truncates a tuned float — the param type is int).
func (m pmap) integer(name string) (int64, error) {
	f, err := m.float(name)
	if err != nil {
		return 0, err
	}
	if f != float64(int64(f)) {
		return 0, fmt.Errorf("parameter %q: expected integer, got %v", name, f)
	}
	return int64(f), nil
}

func (m pmap) str(name string) (string, error) {
	v, ok := m[name]
	if !ok {
		return "", fmt.Errorf("missing parameter %q", name)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("parameter %q: expected string, got %T", name, v)
	}
	return s, nil
}

func (m pmap) strList(name string) ([]string, error) {
	v, ok := m[name]
	if !ok {
		return nil, fmt.Errorf("missing parameter %q", name)
	}
	raw, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("parameter %q: expected list, got %T", name, v)
	}
	out := make([]string, 0, len(raw))
	for i, e := range raw {
		s, ok := e.(string)
		if !ok {
			return nil, fmt.Errorf("parameter %q[%d]: expected string, got %T", name, i, e)
		}
		out = append(out, s)
	}
	return out, nil
}

// pairList decodes the pairs param: a list of 2-element [long, short] lists.
func (m pmap) pairList(name string) ([]Pair, error) {
	v, ok := m[name]
	if !ok {
		return nil, fmt.Errorf("missing parameter %q", name)
	}
	raw, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("parameter %q: expected list, got %T", name, v)
	}
	out := make([]Pair, 0, len(raw))
	for i, e := range raw {
		leg, ok := e.([]any)
		if !ok || len(leg) != 2 {
			return nil, fmt.Errorf("parameter %q[%d]: expected [long, short] pair", name, i)
		}
		long, ok1 := leg[0].(string)
		short, ok2 := leg[1].(string)
		if !ok1 || !ok2 {
			return nil, fmt.Errorf("parameter %q[%d]: legs must be strings", name, i)
		}
		out = append(out, Pair{LongLeg: long, ShortLeg: short})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// typed decode + __post_init__-equivalent validation
// ---------------------------------------------------------------------------

func sepaFromMap(m pmap) (SEPAParams, error) {
	var p SEPAParams
	var err error
	if p.RiskPct, err = m.float("risk_pct"); err != nil {
		return p, err
	}
	if p.MarketCapMinUSD, err = m.float("market_cap_min_usd"); err != nil {
		return p, err
	}
	if p.HardStopPct, err = m.float("hard_stop_pct"); err != nil {
		return p, err
	}
	if p.PivotBufferPct, err = m.float("pivot_buffer_pct"); err != nil {
		return p, err
	}
	if p.BreakoutVolumeMultiple, err = m.float("breakout_volume_multiple"); err != nil {
		return p, err
	}
	if p.VCPLookback, err = m.integer("vcp_lookback"); err != nil {
		return p, err
	}
	if p.HistoryMaxBars, err = m.integer("history_max_bars"); err != nil {
		return p, err
	}
	if p.Timezone, err = m.str("timezone"); err != nil {
		return p, err
	}
	if err := p.Validate(); err != nil {
		return p, err
	}
	return p, nil
}

// Validate mirrors SEPASignalGeneratorConfig.__post_init__ (signal.py:155-163).
func (p SEPAParams) Validate() error {
	if !(p.RiskPct > 0 && p.RiskPct <= 100) {
		return fmt.Errorf("risk_pct must be in (0, 100]")
	}
	if !(p.HardStopPct > 0) {
		return fmt.Errorf("hard_stop_pct must be > 0")
	}
	return nil
}

func pairsFromMap(m pmap) (PairsParams, error) {
	var p PairsParams
	var err error
	if p.Pairs, err = m.pairList("pairs"); err != nil {
		return p, err
	}
	if p.Lookback, err = m.integer("lookback"); err != nil {
		return p, err
	}
	if p.EntryZ, err = m.float("entry_z"); err != nil {
		return p, err
	}
	if p.ExitZ, err = m.float("exit_z"); err != nil {
		return p, err
	}
	if p.CapitalPerPairPct, err = m.float("capital_per_pair_pct"); err != nil {
		return p, err
	}
	if p.Timezone, err = m.str("timezone"); err != nil {
		return p, err
	}
	if err := p.Validate(); err != nil {
		return p, err
	}
	return p, nil
}

// Validate mirrors PairsSignalGeneratorConfig.__post_init__ (signal.py:92-104).
func (p PairsParams) Validate() error {
	if len(p.Pairs) == 0 {
		return fmt.Errorf("pairs must not be empty")
	}
	if p.Lookback < 5 {
		return fmt.Errorf("lookback must be >= 5")
	}
	if p.EntryZ <= 0 || p.ExitZ < 0 {
		return fmt.Errorf("entry_z must be > 0 and exit_z must be >= 0")
	}
	if p.ExitZ >= p.EntryZ {
		return fmt.Errorf("exit_z must be < entry_z (else no entry/exit gap)")
	}
	if !(p.CapitalPerPairPct > 0 && p.CapitalPerPairPct <= 1) {
		return fmt.Errorf("capital_per_pair_pct must be in (0, 1]")
	}
	return nil
}

func sectorFromMap(m pmap) (SectorRotationParams, error) {
	var p SectorRotationParams
	var err error
	if p.Universe, err = m.strList("universe"); err != nil {
		return p, err
	}
	if p.MomentumLookback, err = m.integer("momentum_lookback"); err != nil {
		return p, err
	}
	if p.TopK, err = m.integer("top_k"); err != nil {
		return p, err
	}
	if p.Timezone, err = m.str("timezone"); err != nil {
		return p, err
	}
	if err := p.Validate(); err != nil {
		return p, err
	}
	return p, nil
}

// Validate mirrors SectorRotationSignalGeneratorConfig.__post_init__
// (signal.py:80-92). The top_k message embeds len(universe), exactly as Python.
func (p SectorRotationParams) Validate() error {
	if len(p.Universe) == 0 {
		return fmt.Errorf("universe must not be empty")
	}
	if p.MomentumLookback < 2 {
		return fmt.Errorf("momentum_lookback must be >= 2")
	}
	if !(p.TopK >= 1 && p.TopK <= int64(len(p.Universe))) {
		return fmt.Errorf("top_k must be in [1, %d], got %d", len(p.Universe), p.TopK)
	}
	return nil
}

func intradayFromMap(m pmap) (IntradayBreakoutParams, error) {
	var p IntradayBreakoutParams
	var err error
	if p.RiskPct, err = m.float("risk_pct"); err != nil {
		return p, err
	}
	if p.RangeMinutes, err = m.integer("range_minutes"); err != nil {
		return p, err
	}
	if p.VolMultiple, err = m.float("vol_multiple"); err != nil {
		return p, err
	}
	if p.ProfitTargetR, err = m.float("profit_target_r"); err != nil {
		return p, err
	}
	if p.HardStopPct, err = m.float("hard_stop_pct"); err != nil {
		return p, err
	}
	if p.EODExitTime, err = m.str("eod_exit_time"); err != nil {
		return p, err
	}
	if p.Timezone, err = m.str("timezone"); err != nil {
		return p, err
	}
	if err := p.Validate(); err != nil {
		return p, err
	}
	return p, nil
}

// Validate mirrors IntradayBreakoutSignalGeneratorConfig.__post_init__
// (signal.py:65-91): numeric bounds, eod_exit_time HH:MM parse, IANA timezone.
func (p IntradayBreakoutParams) Validate() error {
	if p.RiskPct <= 0 || p.RiskPct > 100 {
		return fmt.Errorf("risk_pct must be in (0, 100]")
	}
	if p.RangeMinutes < 1 {
		return fmt.Errorf("range_minutes must be >= 1")
	}
	if p.VolMultiple <= 0 {
		return fmt.Errorf("vol_multiple must be > 0")
	}
	if p.ProfitTargetR <= 0 {
		return fmt.Errorf("profit_target_r must be > 0")
	}
	if p.HardStopPct <= 0 || p.HardStopPct > 50 {
		return fmt.Errorf("hard_stop_pct must be in (0, 50]")
	}
	if err := validateHHMM(p.EODExitTime); err != nil {
		return err
	}
	if _, err := time.LoadLocation(p.Timezone); err != nil {
		return fmt.Errorf("timezone %q not recognized", p.Timezone)
	}
	return nil
}

// validateHHMM mirrors the eod_exit_time parse: split on ":", int(h), int(m),
// require 0<=h<=23 and 0<=m<=59. Any parse failure -> "eod_exit_time must be HH:MM".
func validateHHMM(s string) error {
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return fmt.Errorf("eod_exit_time must be HH:MM")
	}
	h, err1 := strconv.Atoi(parts[0])
	mnt, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return fmt.Errorf("eod_exit_time must be HH:MM")
	}
	if !(h >= 0 && h <= 23 && mnt >= 0 && mnt <= 59) {
		return fmt.Errorf("eod_exit_time must be HH:MM")
	}
	return nil
}
