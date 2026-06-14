package strategyassembly

// gate_test.go locks the single-strategy gate-selection contract introduced for
// P4 objective parity (FIXER round 2, finding 1):
//
//   - Input.MultiStrategyGate == false (default, the P2/P3 backtest path) -> a
//     single strategy gets the LONE-strategy gate: 100% allocator budget + the
//     reference DEFAULT risk caps (single-name 20%, concentration 30%,
//     daily-loss 5%) — EXCEPT SectorRotation, which uses its CANONICAL caps
//     (single-name 50%, concentration 40%, daily-loss 10%): a topK rotation holds
//     1/topK (33% at topK=3) per name, structurally impossible under a 20%
//     single-name cap, so the lone strategy would trade NOTHING (FIXER round 2,
//     finding 1; confirmed against the Python oracle, which never runs
//     SectorRotation under any other caps).
//   - Input.MultiStrategyGate == true  (the P4 hyperopt objective path) -> a
//     single strategy gets its CANONICAL MULTI-strategy slice (SEPA 40 / Sector
//     30 / Pairs 20) + the multi risk caps (single-name 50%, concentration 40%,
//     daily-loss 10%), exactly as scripts/multi_strategy_backtest.run_backtest
//     always installs. The OTHER two strategy ids are registered in the allocator
//     (so an unselected id would be budgeted, not rejected-as-unregistered) even
//     though only the selected strategy actually trades.
//
// This is the root-cause guard for the objective-parity blocker: the wrong gate
// admits/rejects a different order set and the objective vector diverges from
// Python.

import (
	"testing"

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

func baseSectorParams() params.SectorRotationParams {
	return params.SectorRotationParams{
		Universe:         []string{"XLK", "XLF", "XLE"},
		MomentumLookback: 60,
		TopK:             1,
		Timezone:         "America/New_York",
	}
}

func assembleSingle(t *testing.T, strategy string, multiGate bool) *Assembly {
	t.Helper()
	in := Input{
		Strategy:          strategy,
		StartingBalance:   100000,
		MultiStrategyGate: multiGate,
	}
	switch strategy {
	case "pairs":
		in.Params.Pairs = basePairsParams()
	case "sector_rotation":
		in.Params.Sector = baseSectorParams()
	}
	asm, err := Assemble(in)
	if err != nil {
		t.Fatalf("Assemble(%s, multiGate=%v): %v", strategy, multiGate, err)
	}
	return asm
}

func TestSingleStrategyGateLoneBudget(t *testing.T) {
	// Non-sector lone strategies keep the generic default caps (their P4 parity
	// contract). Pairs is the representative case.
	for _, strategy := range []string{"pairs"} {
		t.Run(strategy, func(t *testing.T) {
			asm := assembleSingle(t, strategy, false)
			pf := asm.Portfolio
			id := selectedID(strategy)

			if got := pf.Allocator().BudgetPct(id); got != 1.0 {
				t.Fatalf("lone gate: budget for %s = %v, want 1.0", id, got)
			}
			rc := pf.RiskConstraints().Config()
			if rc.MaxSingleNamePct != 0.20 || rc.ConcentrationPct != 0.30 || rc.DailyLossHaltPct != 0.05 {
				t.Fatalf("lone gate risk caps = %+v, want default (0.20/0.30/0.05)", rc)
			}
		})
	}
}

// TestLoneSectorGateUsesCanonicalCaps locks the SectorRotation-specific lone gate
// (FIXER round 2, finding 1): full book (100% budget) under the canonical sector
// caps (single-name 50%, concentration 40%, daily-loss 10%), NOT the generic
// 20/30/5 default. A topK rotation holds 1/topK per name (33% at topK=3); the
// generic 20% single-name cap would reject every pick and the default live
// profile strategy would trade nothing.
func TestLoneSectorGateUsesCanonicalCaps(t *testing.T) {
	asm := assembleSingle(t, "sector_rotation", false)
	pf := asm.Portfolio

	if got := pf.Allocator().BudgetPct(IDSector); got != 1.0 {
		t.Fatalf("lone sector gate: budget = %v, want 1.0 (full book)", got)
	}
	rc := pf.RiskConstraints().Config()
	if rc.MaxSingleNamePct != sectorMaxSingleName || rc.ConcentrationPct != sectorConcentration || rc.DailyLossHaltPct != sectorDailyLossHalt {
		t.Fatalf("lone sector gate caps = %+v, want canonical (0.50/0.40/0.10)", rc)
	}
}

func TestSingleStrategyGateMultiSlice(t *testing.T) {
	cases := map[string]float64{
		"pairs":           allocPairs,  // 0.20
		"sector_rotation": allocSector, // 0.30
	}
	for strategy, wantBudget := range cases {
		t.Run(strategy, func(t *testing.T) {
			asm := assembleSingle(t, strategy, true)
			pf := asm.Portfolio
			id := selectedID(strategy)

			if got := pf.Allocator().BudgetPct(id); got != wantBudget {
				t.Fatalf("multi gate: budget for %s = %v, want %v (canonical multi slice)", id, got, wantBudget)
			}
			// The other two daily-strategy ids must also be registered so an
			// unselected id is budgeted (not rejected as unregistered), matching
			// Python building all three runners under one allocator.
			for _, other := range []string{IDSEPA, IDSector, IDPairs} {
				if got := pf.Allocator().BudgetPct(other); got <= 0 {
					t.Fatalf("multi gate: id %s not registered in allocator (budget=%v)", other, got)
				}
			}
			rc := pf.RiskConstraints().Config()
			if rc.MaxSingleNamePct != riskMaxSingleName || rc.ConcentrationPct != riskConcentration || rc.DailyLossHaltPct != riskDailyLossHalt {
				t.Fatalf("multi gate risk caps = %+v, want multi (0.50/0.40/0.10)", rc)
			}
		})
	}
}

// selectedID maps the assembly strategy selector to its canonical engine id.
func selectedID(strategy string) string {
	switch strategy {
	case "pairs":
		return IDPairs
	case "sector_rotation":
		return IDSector
	case "sepa":
		return IDSEPA
	default:
		return ""
	}
}
