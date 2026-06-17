package study

import (
	"math"
	"testing"

	"github.com/byjackchen/trade-tms-go/internal/composition"
	"github.com/byjackchen/trade-tms-go/internal/hyperopt/nsga2"
)

// targetComposition is a 3-active-member blueprint with a known param_set_id on one
// member, used to prove decode preserves member identity (decision 4).
func targetComposition() composition.Composition {
	ps := int64(42)
	return composition.Composition{
		ID:      "blend",
		Name:    "Blend",
		CashPct: 0.10,
		Risk:    composition.Risk{SingleNamePct: 0.50, ConcentrationPct: 0.40, DailyLossHaltPct: 0.10},
		Members: []composition.Member{
			{StrategyID: composition.StrategySEPA, Weight: 0.40, Active: true},
			{StrategyID: composition.StrategySectorRotation, Weight: 0.30, Active: true, ParamSetID: &ps},
			{StrategyID: composition.StrategyPairs, Weight: 0.20, Active: false}, // inactive: no weight dim, dropped
		},
		Version: 3,
	}
}

func TestCompositionSpaceDimsAndDefaults(t *testing.T) {
	cs, err := NewCompositionSpace(targetComposition(), DefaultCompositionRanges())
	if err != nil {
		t.Fatalf("NewCompositionSpace: %v", err)
	}
	// Active members: sepa, sector_rotation (pairs inactive). Dims: 2 weights + cash
	// + 3 risk = 6.
	if got := cs.Space().Len(); got != 6 {
		t.Fatalf("dim count = %d, want 6 (2 active weights + cash + 3 risk)", got)
	}
	for _, name := range []string{
		compWeightPrefix + composition.StrategySEPA,
		compWeightPrefix + composition.StrategySectorRotation,
		compCashDim, compRiskSingle, compRiskConc, compRiskDaily,
	} {
		if cs.Space().Index(name) < 0 {
			t.Errorf("missing search dim %q", name)
		}
	}
	// Inactive pairs member must NOT have a weight dim.
	if cs.Space().Index(compWeightPrefix+composition.StrategyPairs) >= 0 {
		t.Error("inactive member pairs must not carry a weight dim")
	}
}

func TestCompositionSpaceDecodeNormalizesSimplex(t *testing.T) {
	cs, err := NewCompositionSpace(targetComposition(), DefaultCompositionRanges())
	if err != nil {
		t.Fatalf("NewCompositionSpace: %v", err)
	}
	cand := nsga2.Params{
		compWeightPrefix + composition.StrategySEPA:           0.6,
		compWeightPrefix + composition.StrategySectorRotation: 0.2,
		compCashDim:    0.2,
		compRiskSingle: 0.33,
		compRiskConc:   0.44,
		compRiskDaily:  0.07,
	}
	dec, err := cs.Decode(cand)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if dec.Composition == nil {
		t.Fatal("decoded composition is nil")
	}
	m := dec.Composition

	// Simplex: Σweights + cash == 1 exactly. raw sum = 0.6+0.2+0.2 = 1.0, so the
	// normalized values equal the raws here, but the invariant must hold regardless.
	var sumW float64
	for _, mem := range m.Members {
		sumW += mem.Weight
	}
	if d := math.Abs(sumW + m.CashPct - 1.0); d > 1e-12 {
		t.Fatalf("Σweights + cash = %v, want 1.0 (diff %v)", sumW+m.CashPct, d)
	}
	// Only the 2 ACTIVE members survive, identity preserved (sector keeps param_set_id=42).
	if len(m.Members) != 2 {
		t.Fatalf("decoded members = %d, want 2 (active only)", len(m.Members))
	}
	byID := map[string]composition.Member{}
	for _, mem := range m.Members {
		byID[mem.StrategyID] = mem
	}
	if mem, ok := byID[composition.StrategySectorRotation]; !ok || mem.ParamSetID == nil || *mem.ParamSetID != 42 {
		t.Errorf("sector member must keep param_set_id=42 (decision 4); got %+v", mem)
	}
	if _, ok := byID[composition.StrategyPairs]; ok {
		t.Error("inactive pairs member must not appear in the decoded blueprint")
	}
	// Risk maps straight through.
	if m.Risk.SingleNamePct != 0.33 || m.Risk.ConcentrationPct != 0.44 || m.Risk.DailyLossHaltPct != 0.07 {
		t.Errorf("risk = %+v, want 0.33/0.44/0.07", m.Risk)
	}
	// The decoded blueprint must be valid (feasible) for assembly.
	if err := m.Validate(); err != nil {
		t.Errorf("decoded composition invalid: %v", err)
	}
}

func TestCompositionSpaceDecodeAllZeroFallback(t *testing.T) {
	// Ranges allowing a 0 raw weight + 0 cash draw: the degenerate point must still
	// decode to a feasible equal-weight simplex (never a rejected trial).
	ranges := DefaultCompositionRanges()
	ranges.WeightLow = 0.0
	cs, err := NewCompositionSpace(targetComposition(), ranges)
	if err != nil {
		t.Fatalf("NewCompositionSpace: %v", err)
	}
	cand := nsga2.Params{
		compWeightPrefix + composition.StrategySEPA:           0.0,
		compWeightPrefix + composition.StrategySectorRotation: 0.0,
		compCashDim:    0.0,
		compRiskSingle: 0.30, compRiskConc: 0.40, compRiskDaily: 0.05,
	}
	dec, err := cs.Decode(cand)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	m := dec.Composition
	var sumW float64
	for _, mem := range m.Members {
		sumW += mem.Weight
	}
	if d := math.Abs(sumW + m.CashPct - 1.0); d > 1e-12 {
		t.Fatalf("degenerate decode Σweights+cash = %v, want 1.0", sumW+m.CashPct)
	}
	if err := m.Validate(); err != nil {
		t.Errorf("degenerate decode invalid: %v", err)
	}
}

func TestCompositionSpaceRecordedParams(t *testing.T) {
	cs, err := NewCompositionSpace(targetComposition(), DefaultCompositionRanges())
	if err != nil {
		t.Fatalf("NewCompositionSpace: %v", err)
	}
	cand := nsga2.Params{
		compWeightPrefix + composition.StrategySEPA:           0.5,
		compWeightPrefix + composition.StrategySectorRotation: 0.3,
		compCashDim:    0.2,
		compRiskSingle: 0.30, compRiskConc: 0.40, compRiskDaily: 0.05,
	}
	rec := cs.RecordedParams(cand)
	// The recorded shape is what promote reads: cash + 3 risk caps + a weights map.
	for _, k := range []string{"cash_pct", "single_name_pct", "concentration_pct", "daily_loss_halt_pct", "weights"} {
		if _, ok := rec[k]; !ok {
			t.Errorf("recorded params missing %q", k)
		}
	}
	weights, ok := rec["weights"].(map[string]any)
	if !ok {
		t.Fatalf("weights is %T, want map[string]any", rec["weights"])
	}
	if _, ok := weights[composition.StrategySEPA]; !ok {
		t.Error("weights missing sepa")
	}
	if _, ok := weights[composition.StrategyPairs]; ok {
		t.Error("weights must not include the inactive pairs member")
	}
}

func TestNewCompositionSpaceRejectsNoActiveMembers(t *testing.T) {
	c := targetComposition()
	for i := range c.Members {
		c.Members[i].Active = false
	}
	// Validate would already reject (no active budget is fine, but our space needs
	// >=1 active member to tune).
	if _, err := NewCompositionSpace(c, DefaultCompositionRanges()); err == nil {
		t.Fatal("expected error for a composition with no active members")
	}
}
