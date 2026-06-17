package hyperopt

import (
	"math"
	"testing"

	"github.com/byjackchen/trade-tms-go/internal/composition"
	"github.com/byjackchen/trade-tms-go/internal/hyperopt/nsga2"
)

const normEpsilon = 1e-9

func sampleComp() composition.Composition {
	ps := int64(7)
	return composition.Composition{
		ID:      "c1",
		Name:    "C1",
		CashPct: 0.10,
		Risk:    composition.Risk{SingleNamePct: 0.5, ConcentrationPct: 0.4, DailyLossHaltPct: 0.1},
		Members: []composition.Member{
			{StrategyID: composition.StrategySEPA, Weight: 0.4, Active: true, ParamSetID: &ps},
			{StrategyID: composition.StrategyPairs, Weight: 0.3, Active: true},
			{StrategyID: composition.StrategySectorRotation, Weight: 0.2, Active: false},
		},
		Version: 1,
	}
}

// TestNewCompositionSpaceDims: one weight dim per ACTIVE member + cash + 3 risk.
func TestNewCompositionSpaceDims(t *testing.T) {
	s, err := NewCompositionSpace(sampleComp(), DefaultCompositionRanges())
	if err != nil {
		t.Fatalf("NewCompositionSpace: %v", err)
	}
	// 2 active members + cash + 3 risk = 6 dims.
	if got := s.Space().Len(); got != 6 {
		t.Fatalf("Space().Len() = %d, want 6", got)
	}
	for _, name := range []string{
		compWeightPrefix + composition.StrategySEPA,
		compWeightPrefix + composition.StrategyPairs,
		compCashName, compRiskSingleName, compRiskConcentNm, compRiskDailyHaltNm,
	} {
		if s.Space().Index(name) < 0 {
			t.Errorf("missing dimension %q", name)
		}
	}
	// Inactive member must NOT get a weight dim.
	if s.Space().Index(compWeightPrefix+composition.StrategySectorRotation) >= 0 {
		t.Errorf("inactive member got a weight dim")
	}
}

func TestNewCompositionSpaceNoActive(t *testing.T) {
	comp := sampleComp()
	for i := range comp.Members {
		comp.Members[i].Active = false
	}
	if _, err := NewCompositionSpace(comp, DefaultCompositionRanges()); err == nil {
		t.Fatal("expected error for no active members")
	}
}

// TestDecodeNormalizesSimplex: Σ(weights) + cash == 1 within epsilon (decision 1a)
// and risk passes through verbatim, across several raw vectors.
func TestDecodeNormalizesSimplex(t *testing.T) {
	s, err := NewCompositionSpace(sampleComp(), DefaultCompositionRanges())
	if err != nil {
		t.Fatalf("NewCompositionSpace: %v", err)
	}

	cases := []struct {
		name              string
		wSEPA, wPairs     float64
		cash              float64
		single, con, halt float64
	}{
		{"balanced", 0.5, 0.5, 0.1, 0.30, 0.40, 0.08},
		{"weight-heavy", 1.0, 0.05, 0.0, 0.10, 0.20, 0.02},
		{"cash-heavy", 0.05, 0.05, 0.3, 0.60, 0.60, 0.15},
		{"asymmetric", 0.9, 0.1, 0.2, 0.25, 0.55, 0.12},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cand := nsga2.Params{
				compWeightPrefix + composition.StrategySEPA:  c.wSEPA,
				compWeightPrefix + composition.StrategyPairs: c.wPairs,
				compCashName:        c.cash,
				compRiskSingleName:  c.single,
				compRiskConcentNm:   c.con,
				compRiskDailyHaltNm: c.halt,
			}
			out, err := s.DecodeComposition(cand)
			if err != nil {
				t.Fatalf("DecodeComposition: %v", err)
			}

			sum := out.CashPct
			for _, m := range out.Members {
				sum += m.Weight
				if !m.Active {
					t.Errorf("decoded member %q not active", m.StrategyID)
				}
			}
			if math.Abs(sum-1.0) > normEpsilon {
				t.Errorf("Σ(weights)+cash = %v, want 1.0 (±%g)", sum, normEpsilon)
			}

			// Ratio preserved: weight_SEPA/weight_Pairs == wSEPA/wPairs.
			var wSEPA, wPairs float64
			for _, m := range out.Members {
				switch m.StrategyID {
				case composition.StrategySEPA:
					wSEPA = m.Weight
				case composition.StrategyPairs:
					wPairs = m.Weight
				}
			}
			if got, want := wSEPA/wPairs, c.wSEPA/c.wPairs; math.Abs(got-want) > 1e-9 {
				t.Errorf("weight ratio = %v, want %v", got, want)
			}

			// Risk passes through verbatim.
			if out.Risk.SingleNamePct != c.single || out.Risk.ConcentrationPct != c.con || out.Risk.DailyLossHaltPct != c.halt {
				t.Errorf("risk = %+v, want single=%v con=%v halt=%v", out.Risk, c.single, c.con, c.halt)
			}

			// Decoded result must be a valid Composition.
			if err := out.Validate(); err != nil {
				t.Errorf("decoded composition invalid: %v", err)
			}
		})
	}
}

// TestDecodeFixedCarryThrough: ParamSetID, id/name, inactive drop, optional risk
// caps are carried through unchanged (decision 4).
func TestDecodeFixedCarryThrough(t *testing.T) {
	comp := sampleComp()
	maxGross := 1.5
	maxPos := 12
	comp.Risk.MaxGrossPct = &maxGross
	comp.Risk.MaxPositions = &maxPos

	s, err := NewCompositionSpace(comp, DefaultCompositionRanges())
	if err != nil {
		t.Fatalf("NewCompositionSpace: %v", err)
	}
	cand := nsga2.Params{
		compWeightPrefix + composition.StrategySEPA:  0.4,
		compWeightPrefix + composition.StrategyPairs: 0.6,
		compCashName:        0.1,
		compRiskSingleName:  0.3,
		compRiskConcentNm:   0.4,
		compRiskDailyHaltNm: 0.1,
	}
	out, err := s.DecodeComposition(cand)
	if err != nil {
		t.Fatalf("DecodeComposition: %v", err)
	}
	if out.ID != "c1" || out.Name != "C1" {
		t.Errorf("identity not carried through: id=%q name=%q", out.ID, out.Name)
	}
	if len(out.Members) != 2 {
		t.Fatalf("decoded %d members, want 2 (inactive dropped)", len(out.Members))
	}
	for _, m := range out.Members {
		if m.StrategyID == composition.StrategySEPA {
			if m.ParamSetID == nil || *m.ParamSetID != 7 {
				t.Errorf("SEPA ParamSetID not carried through: %v", m.ParamSetID)
			}
		}
		if m.StrategyID == composition.StrategySectorRotation {
			t.Errorf("inactive member leaked into decoded result")
		}
	}
	if out.Risk.MaxGrossPct == nil || *out.Risk.MaxGrossPct != maxGross {
		t.Errorf("MaxGrossPct not carried through: %v", out.Risk.MaxGrossPct)
	}
	if out.Risk.MaxPositions == nil || *out.Risk.MaxPositions != maxPos {
		t.Errorf("MaxPositions not carried through: %v", out.Risk.MaxPositions)
	}
}

func TestDecodeMissingDimension(t *testing.T) {
	s, err := NewCompositionSpace(sampleComp(), DefaultCompositionRanges())
	if err != nil {
		t.Fatalf("NewCompositionSpace: %v", err)
	}
	// Omit cash.
	cand := nsga2.Params{
		compWeightPrefix + composition.StrategySEPA:  0.4,
		compWeightPrefix + composition.StrategyPairs: 0.6,
		compRiskSingleName:                           0.3,
		compRiskConcentNm:                            0.4,
		compRiskDailyHaltNm:                          0.1,
	}
	if _, err := s.DecodeComposition(cand); err == nil {
		t.Fatal("expected error for missing cash dimension")
	}
}

// TestRangesPersisted: the ranges used are exposed for search_config persistence.
func TestRangesPersisted(t *testing.T) {
	custom := DefaultCompositionRanges()
	custom.RawCash = SearchSpec{Low: 0.0, High: 0.5}
	s, err := NewCompositionSpace(sampleComp(), custom)
	if err != nil {
		t.Fatalf("NewCompositionSpace: %v", err)
	}
	if s.Ranges().RawCash.High != 0.5 {
		t.Errorf("Ranges().RawCash.High = %v, want 0.5", s.Ranges().RawCash.High)
	}
}
