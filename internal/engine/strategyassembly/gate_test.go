package strategyassembly

// gate_test.go locks the Model-driven gate-selection contract: the allocator
// budgets and the risk constraints now come from the Model (its ACTIVE members'
// weights -> allocator budgets, and model.Risk -> risk caps), NOT from hardcoded
// constants or a MultiStrategyGate parity flag (which is gone — parity is
// abandoned, docs/concept-alignment.md §3.2, D1).
//
//   - A single-member Model (sepa/pairs/orb-only) gates its lone strategy at its
//     OWN member risk: weight 1.0 budget + the member's risk caps (sepa/pairs/orb
//     0.20/0.30/0.05; sector 0.50/0.40/0.10).
//   - The default-multi Model reproduces the old "multi" gate: SEPA 40 / Sector 30
//     / Pairs 20 with multi risk caps 0.50/0.40/0.10.

import (
	"testing"

	"github.com/byjackchen/trade-tms-go/internal/model"
	"github.com/byjackchen/trade-tms-go/internal/params"
)

func basePairsParams() params.PairsParams {
	return params.PairsParams{
		Pairs:             []params.Pair{{LongLeg: "KO", ShortLeg: "PEP"}},
		Lookback:          60,
		EntryZ:            2.0,
		ExitZ:             0.5,
		CapitalPerPairPct: 0.3,
		Timezone:          "America/New_York",
	}
}

func baseSEPAParams() params.SEPAParams {
	return params.SEPAParams{
		RiskPct: 1.0, MarketCapMinUSD: 5e8, HardStopPct: 7.5, PivotBufferPct: 1.5,
		BreakoutVolumeMultiple: 1.5, VCPLookback: 4, HistoryMaxBars: 1000,
		Timezone: "America/New_York",
	}
}

func baseSectorParams() params.SectorRotationParams {
	return params.SectorRotationParams{
		Universe:         []string{"XLK", "XLF", "XLE"},
		MomentumLookback: 60,
		TopK:             1,
		Timezone:         "America/New_York",
	}
}

// assembleModel builds an Assembly from the named seed model with the params the
// model's members need populated.
func assembleModel(t *testing.T, modelID string) *Assembly {
	t.Helper()
	mdl, err := model.Seed(modelID)
	if err != nil {
		t.Fatalf("model.Seed(%s): %v", modelID, err)
	}
	in := Input{
		Model:           mdl,
		StartingBalance: 100000,
		SEPAStocks:      []string{"AAA"},
		Params: Params{
			SEPA:   baseSEPAParams(),
			Sector: baseSectorParams(),
			Pairs:  basePairsParams(),
		},
	}
	asm, err := Assemble(in)
	if err != nil {
		t.Fatalf("Assemble(%s): %v", modelID, err)
	}
	return asm
}

// TestSingleMemberModelUsesOwnRisk locks that a single-member Model gates its
// lone strategy at weight 1.0 budget + the member's own risk caps (no longer the
// generic-default vs canonical-sector special-casing — risk is Model DATA now).
func TestSingleMemberModelUsesOwnRisk(t *testing.T) {
	cases := []struct {
		modelID   string
		id        string
		single    float64
		conc      float64
		dailyHalt float64
	}{
		{"pairs-only", IDPairs, 0.20, 0.30, 0.05},
		{"sepa-only", IDSEPA, 0.20, 0.30, 0.05},
		{"sector-only", IDSector, 0.50, 0.40, 0.10},
	}
	for _, c := range cases {
		t.Run(c.modelID, func(t *testing.T) {
			asm := assembleModel(t, c.modelID)
			pf := asm.Gate
			if got := pf.Allocator().BudgetPct(c.id); got != 1.0 {
				t.Fatalf("single-member gate: budget for %s = %v, want 1.0", c.id, got)
			}
			rc := pf.RiskConstraints().Config()
			if rc.MaxSingleNamePct != c.single || rc.ConcentrationPct != c.conc || rc.DailyLossHaltPct != c.dailyHalt {
				t.Fatalf("single-member gate caps = %+v, want (%v/%v/%v)", rc, c.single, c.conc, c.dailyHalt)
			}
		})
	}
}

// TestDefaultMultiModelReproducesMultiGate locks the behaviour contract: the
// default-multi Model reproduces the old "multi" gate exactly — SEPA 0.40 /
// Sector 0.30 / Pairs 0.20 allocator budgets and risk caps 0.50/0.40/0.10.
func TestDefaultMultiModelReproducesMultiGate(t *testing.T) {
	asm := assembleModel(t, "default-multi")
	pf := asm.Gate

	wantBudget := map[string]float64{IDSEPA: 0.40, IDSector: 0.30, IDPairs: 0.20}
	for id, want := range wantBudget {
		if got := pf.Allocator().BudgetPct(id); got != want {
			t.Fatalf("default-multi gate: budget for %s = %v, want %v", id, got, want)
		}
	}
	rc := pf.RiskConstraints().Config()
	if rc.MaxSingleNamePct != 0.50 || rc.ConcentrationPct != 0.40 || rc.DailyLossHaltPct != 0.10 {
		t.Fatalf("default-multi gate risk caps = %+v, want multi (0.50/0.40/0.10)", rc)
	}
}
