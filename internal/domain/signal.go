package domain

// signal.go defines Signal (the target-position signal emitted by every
// SignalGenerator), the SEPA Grade model and VCPSnapshot, mirroring the
// frozen Python dataclasses (spec §2.3-§2.5).

import (
	"fmt"
	"time"
)

// Signal is the target-position-style signal emitted by every
// SignalGenerator (spec §2.3 [MUST-MATCH]). Immutable by convention.
//
// Field semantics:
//   - TargetQty is signed by convention (positive long, negative short,
//     0 flat). All emitters pass positive magnitudes for LONG/SHORT and 0
//     for FLAT — except ORB's FLAT, which carries the *held* qty (the
//     runners ignore TargetQty on FLAT either way, but the value is visible
//     in logs/serialized intents and must be preserved).
//   - Confidence defaults to 1.0 and is currently never set differently.
//   - Grade is nil unless SEPA set it; StopPrice is set by SEPA entry/exit
//     and ORB entry only.
type Signal struct {
	Symbol     string     `json:"symbol"`
	TS         time.Time  `json:"ts"`
	Side       SignalSide `json:"side"`
	TargetQty  Qty        `json:"target_qty"`
	Reason     string     `json:"reason"`
	Confidence float64    `json:"confidence"`
	Grade      *Grade     `json:"grade"`
	StopPrice  *Price     `json:"stop_price"`
}

// NewSignal builds a Signal with the Python default Confidence of 1.0 and
// Grade/StopPrice unset. Set the optional fields on the returned value
// before first use.
func NewSignal(symbol string, ts time.Time, side SignalSide, targetQty Qty, reason string) Signal {
	return Signal{
		Symbol:     symbol,
		TS:         ts,
		Side:       side,
		TargetQty:  targetQty,
		Reason:     reason,
		Confidence: 1.0,
	}
}

// Validate checks structural invariants (opt-in; the Python reference does
// not validate on construction).
func (s Signal) Validate() error {
	if s.Symbol == "" {
		return fmt.Errorf("%w: signal has empty symbol", ErrInvalidArgument)
	}
	if s.TS.IsZero() {
		return fmt.Errorf("%w: signal %s has zero timestamp", ErrInvalidArgument, s.Symbol)
	}
	if !s.Side.IsValid() {
		return fmt.Errorf("%w: signal %s has invalid side %q", ErrInvalidArgument, s.Symbol, string(s.Side))
	}
	if s.Grade != nil && !s.Grade.IsValid() {
		return fmt.Errorf("%w: signal %s has invalid grade %q", ErrInvalidArgument, s.Symbol, string(*s.Grade))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Grade — src/strategies/sepa/grade.py:13 [MUST-MATCH]
// ---------------------------------------------------------------------------

// Grade is the SEPA setup grade: Literal["A+", "B", "skip"] in the Python
// reference.
type Grade string

const (
	GradeAPlus Grade = "A+"
	GradeB     Grade = "B"
	GradeSkip  Grade = "skip"
)

// IsValid reports whether g is a known Grade.
func (g Grade) IsValid() bool {
	switch g {
	case GradeAPlus, GradeB, GradeSkip:
		return true
	}
	return false
}

// String returns the exact Python literal value.
func (g Grade) String() string { return string(g) }

// ParseGrade validates and returns the Grade for s.
func ParseGrade(s string) (Grade, error) {
	v := Grade(s)
	if !v.IsValid() {
		return "", fmt.Errorf("%w: unknown Grade %q", ErrInvalidArgument, s)
	}
	return v, nil
}

// MarshalText implements encoding.TextMarshaler.
func (g Grade) MarshalText() ([]byte, error) { return []byte(g), nil }

// UnmarshalText implements encoding.TextUnmarshaler with validation.
func (g *Grade) UnmarshalText(b []byte) error {
	v, err := ParseGrade(string(b))
	if err != nil {
		return err
	}
	*g = v
	return nil
}

// GradePtr returns a pointer to g, for populating optional Grade fields.
func GradePtr(g Grade) *Grade { return &g }

// ---------------------------------------------------------------------------
// SetupInputs & GradeSetup — src/strategies/sepa/grade.py:16-42 [MUST-MATCH]
// ---------------------------------------------------------------------------

// SetupInputs are the inputs to the canonical Minervini grading rules.
// Stage and Regime stay strings, matching the Python dataclass (the grader
// compares Stage against the literal "2" and Regime against "bear"/"bull";
// any other spelling simply fails those comparisons, exactly as in Python).
type SetupInputs struct {
	TrendTemplatePass   bool   `json:"trend_template_pass"`
	EarningsPass        bool   `json:"earnings_pass"`
	Stage               string `json:"stage"`
	Catalyst            bool   `json:"catalyst"`
	VCPContractionCount int    `json:"vcp_contraction_count"`
	Regime              string `json:"regime"`
}

// GradeSetup returns "A+", "B" or "skip" per the canonical gating rules
// (grade.py:26-42 [MUST-MATCH]), evaluated strictly in order:
//  1. bear regime or stage != "2"                          → skip
//  2. not (trend template pass AND earnings pass)          → skip
//  3. fewer than 2 VCP contractions                        → skip
//  4. catalyst AND >= 3 contractions AND bull regime       → A+
//  5. otherwise                                            → B
func GradeSetup(in SetupInputs) Grade {
	if in.Regime == string(RegimeBear) || in.Stage != "2" {
		return GradeSkip
	}
	if !(in.TrendTemplatePass && in.EarningsPass) {
		return GradeSkip
	}
	if in.VCPContractionCount < 2 {
		return GradeSkip
	}
	if in.Catalyst && in.VCPContractionCount >= 3 && in.Regime == string(RegimeBull) {
		return GradeAPlus
	}
	return GradeB
}

// ---------------------------------------------------------------------------
// VCPSnapshot — src/strategies/sepa/vcp.py:37-51 (spec §2.5)
// ---------------------------------------------------------------------------

// VCPSnapshot describes a detected volatility-contraction pattern. The
// detection algorithm lives in the strategies layer; only the value type is
// defined here. Pivot and depth fields are float64 because the Python
// reference computes them in IEEE-754 float math (spec §1 [MUST-MATCH] —
// do not "upgrade" them to fixed point).
type VCPSnapshot struct {
	Code                         string    `json:"code"`
	Contractions                 []float64 `json:"contractions"` // depths %, oldest→newest
	LastContractionPct           float64   `json:"last_contraction_pct"`
	PivotPrice                   float64   `json:"pivot_price"`
	BaseLengthDays               int       `json:"base_length_days"`
	VolumeDryup                  bool      `json:"volume_dryup"`
	QualityScore                 float64   `json:"quality_score"`
	VolDryupRatio                float64   `json:"vol_dryup_ratio"`
	FinalContractionDurationDays int       `json:"final_contraction_duration_days"`
}
