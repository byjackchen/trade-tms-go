package params_test

// baseline_test.go asserts the resolved typed params match the EXACT values the
// Python reference loader produces for the shipped baseline JSONs. The golden
// values were captured by running
//
//	.venv/bin/python -c "from strategies.params.loader import \
//	  load_strategy_params, defaults_dict; ..."
//
// against src/strategies/params/baseline/*.json. The same JSON files are copied
// verbatim into testdata/ and loaded here via a file-dir Resolver, so a drift in
// either the JSON or the Go decode is caught.

import (
	"context"
	"reflect"
	"testing"

	"github.com/byjackchen/trade-tms-go/internal/params"
)

func testdataLoader() *params.Loader {
	// db=nil, file dir=testdata -> testdata JSON wins, falls back to embedded
	// baseline for any strategy absent from testdata (none, here).
	return params.NewLoader(nil, "testdata")
}

func TestSEPABaselineMatchesPython(t *testing.T) {
	got, doc, err := testdataLoader().SEPA(context.Background())
	if err != nil {
		t.Fatalf("SEPA: %v", err)
	}
	want := params.SEPAParams{
		RiskPct:                1.0,
		MarketCapMinUSD:        500000000.0,
		HardStopPct:            7.5,
		PivotBufferPct:         1.5,
		BreakoutVolumeMultiple: 1.5,
		VCPLookback:            5,
		HistoryMaxBars:         1000,
		Timezone:               "America/New_York",
	}
	if got != want {
		t.Fatalf("SEPA params mismatch:\n got %+v\nwant %+v", got, want)
	}
	if doc.Source != params.OriginFile {
		t.Errorf("source = %q, want file", doc.Source)
	}
	if doc.Params.SchemaVersion != 1 {
		t.Errorf("schema_version = %d, want 1", doc.Params.SchemaVersion)
	}
	if pct, ok := doc.CapitalPct(); !ok || pct != 0.40 {
		t.Errorf("allocation.capital_pct = (%v,%v), want (0.40,true)", pct, ok)
	}
	if !doc.Active() {
		t.Errorf("sepa should be active")
	}
}

func TestPairsBaselineMatchesPython(t *testing.T) {
	got, doc, err := testdataLoader().Pairs(context.Background())
	if err != nil {
		t.Fatalf("Pairs: %v", err)
	}
	want := params.PairsParams{
		Pairs: []params.Pair{
			{LongLeg: "KO", ShortLeg: "PEP"},
			{LongLeg: "MA", ShortLeg: "V"},
			{LongLeg: "XOM", ShortLeg: "CVX"},
		},
		Lookback:          60,
		EntryZ:            2.0,
		ExitZ:             0.5,
		CapitalPerPairPct: 0.30,
		Timezone:          "America/New_York",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Pairs params mismatch:\n got %+v\nwant %+v", got, want)
	}
	// The pairs baseline carries a clamp_high constraint on exit_z.
	if len(doc.Params.Constraints) != 1 {
		t.Fatalf("constraints = %d, want 1", len(doc.Params.Constraints))
	}
	c := doc.Params.Constraints[0]
	if c.Kind != "clamp_high" || c.Param != "exit_z" || c.Expression != "min(1.0, entry_z - 0.1)" {
		t.Errorf("constraint = %+v, want clamp_high/exit_z/min(1.0, entry_z - 0.1)", c)
	}
	if pct, ok := doc.CapitalPct(); !ok || pct != 0.20 {
		t.Errorf("allocation.capital_pct = (%v,%v), want (0.20,true)", pct, ok)
	}
}

func TestSectorRotationBaselineMatchesPython(t *testing.T) {
	got, _, err := testdataLoader().SectorRotation(context.Background())
	if err != nil {
		t.Fatalf("SectorRotation: %v", err)
	}
	want := params.SectorRotationParams{
		Universe:         []string{"XLK", "XLF", "XLE", "XLV", "XLY", "XLP", "XLU", "XLB", "XLI", "XLRE", "XLC"},
		MomentumLookback: 63,
		TopK:             3,
		Timezone:         "America/New_York",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SectorRotation params mismatch:\n got %+v\nwant %+v", got, want)
	}
}

func TestIntradayBreakoutBaselineMatchesPython(t *testing.T) {
	got, doc, err := testdataLoader().IntradayBreakout(context.Background())
	if err != nil {
		t.Fatalf("IntradayBreakout: %v", err)
	}
	want := params.IntradayBreakoutParams{
		RiskPct:       1.0,
		RangeMinutes:  30,
		VolMultiple:   1.5,
		ProfitTargetR: 2.0,
		HardStopPct:   1.0,
		EODExitTime:   "15:55",
		Timezone:      "America/New_York",
	}
	if got != want {
		t.Fatalf("IntradayBreakout params mismatch:\n got %+v\nwant %+v", got, want)
	}
	// intraday_breakout baseline omits the allocation block entirely.
	if _, ok := doc.CapitalPct(); ok {
		t.Errorf("intraday_breakout should have no allocation block")
	}
	if !doc.Active() {
		t.Errorf("absent allocation block -> active by default")
	}
}

// TestEmbeddedBaselineMatchesFileBaseline verifies the embedded (package)
// baseline resolves identically to the testdata copy — i.e. the embedded JSONs
// in internal/hyperopt/baseline are in sync with the Python reference.
func TestEmbeddedBaselineMatchesFileBaseline(t *testing.T) {
	embedded := params.NewLoader(nil, "") // no file dir -> embedded baseline
	file := testdataLoader()
	ctx := context.Background()

	fp, _, err := file.SEPA(ctx)
	if err != nil {
		t.Fatalf("file SEPA: %v", err)
	}
	ep, edoc, err := embedded.SEPA(ctx)
	if err != nil {
		t.Fatalf("embedded SEPA: %v", err)
	}
	if fp != ep {
		t.Fatalf("embedded != file SEPA:\n embedded %+v\n file %+v", ep, fp)
	}
	if edoc.Source != params.OriginBaseline {
		t.Errorf("embedded source = %q, want baseline", edoc.Source)
	}
}
