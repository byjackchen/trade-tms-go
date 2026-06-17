package study

// marketcap_context_test.go proves fix 5 (market-cap context correctness): the
// hyperopt objective's buildContext now seeds REAL, look-ahead-safe market caps
// (from tms.fundamentals_sf1, threaded onto the shared Dataset) into the SEPA
// context provider instead of the old MarketCap=0/HasMarketCap=false stub.
//
// Before the fix every SEPA name read cap 0, failed the rule-8 $500M gate, and
// the SEPA hyperopt/backtest objective degenerated to 0 (never traded). These
// tests pin that:
//   - a large-cap (AAPL) now yields its real non-zero cap via the context's
//     MarketCapAt (proving the stub is gone), and
//   - an unknown ticker (no cap loaded) still reads 0/not-known (the safe
//     "fails rule 8, sorts last" contract), and
//   - the degenerate path is restored ONLY when no caps are threaded in (the
//     pre-fix behaviour), demonstrating the change is real and targeted.

import (
	"testing"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/engine"
)

// sepaContextDataset builds a minimal SEPA dataset: SPY (drives the regime) plus
// the named stocks, daily weekday bars over [start-warmup, end] at a flat price.
// The bars exist only so buildContext finds SPY history; the market caps are
// attached separately via SetMarketCaps.
func sepaContextDataset(t *testing.T, stocks []string, start, end calendar.Date) *Dataset {
	t.Helper()
	syms := append([]string{"SPY"}, stocks...)
	loadStart := start.AddDays(-spyWarmupDays)
	lo := midnight(loadStart)
	hi := midnight(end)

	ibs := make([]engine.InstrumentBars, 0, len(syms))
	for _, sym := range syms {
		var bars []domain.Bar
		day := lo
		for !day.After(hi) {
			if wd := day.Weekday(); wd == time.Saturday || wd == time.Sunday {
				day = day.AddDate(0, 0, 1)
				continue
			}
			p, err := domain.PriceFromFloat64(100.0)
			if err != nil {
				t.Fatal(err)
			}
			bars = append(bars, domain.Bar{
				Symbol: sym, TS: day, Open: p, High: p, Low: p, Close: p, Volume: 1_000_000,
			})
			day = day.AddDate(0, 0, 1)
		}
		ibs = append(ibs, engine.InstrumentBars{Symbol: sym, Bars: bars})
	}
	return NewDatasetFromInstruments(ibs)
}

func sepaContextEvaluator(t *testing.T, ds *Dataset, stocks []string, start, end calendar.Date) *Evaluator {
	t.Helper()
	eval, err := NewEvaluator(EvaluatorConfig{
		Strategy:        "sepa",
		Dataset:         ds,
		Start:           start,
		End:             end,
		SEPAStocks:      stocks,
		StartingBalance: 100000,
		SPYSymbol:       "SPY",
	})
	if err != nil {
		t.Fatalf("NewEvaluator: %v", err)
	}
	return eval
}

// TestBuildContextRealMarketCaps proves buildContext yields the REAL non-zero
// market cap for a known large-cap (AAPL) once caps are threaded onto the
// dataset, and 0/not-known for a ticker with no loaded cap — the production
// backtest handler's exact contract, replacing the old stub.
func TestBuildContextRealMarketCaps(t *testing.T) {
	start := calendar.NewDate(2021, 1, 4)
	end := calendar.NewDate(2021, 6, 30)
	stocks := []string{"AAPL", "TINYCO"}

	ds := sepaContextDataset(t, stocks, start, end)
	// Real-shaped caps: AAPL ~ $2.7T (well above the $500M rule-8 gate), TINYCO
	// unknown (0 -> fails the gate, sorts last).
	const aaplCap = 2_700_000_000_000.0
	ds.SetMarketCaps(map[string]float64{"AAPL": aaplCap, "TINYCO": 0})

	eval := sepaContextEvaluator(t, ds, stocks, start, end)
	cp := eval.buildContext(start, end, true)
	if cp == nil {
		t.Fatal("buildContext returned nil; expected a context provider (SPY bars present)")
	}

	asOf := time.Date(start.Year, start.Month, start.Day, 0, 0, 0, 0, time.UTC)

	v, ok := cp.MarketCapAt("AAPL", asOf)
	if !ok {
		t.Fatal("AAPL market cap must be KNOWN after real caps are loaded (was the old stub: HasMarketCap=false)")
	}
	if got := v.Float64(); got != aaplCap {
		t.Fatalf("AAPL market cap = %v, want %v", got, aaplCap)
	}
	if v.Float64() < 500_000_000 {
		t.Fatalf("AAPL cap %v must clear the $500M rule-8 gate", v.Float64())
	}

	// Unknown cap stays unknown (0): LoadSF1MarketCaps drops rows with cap<=0, so
	// MarketCapAt reports not-known — the safe "fails rule 8" default.
	if _, ok := cp.MarketCapAt("TINYCO", asOf); ok {
		t.Fatal("TINYCO has no loaded cap; MarketCapAt must report not-known (0)")
	}
}

// TestBuildContextDegenerateWithoutCaps pins the pre-fix behaviour as the
// fallback: when NO caps are threaded onto the dataset, every SEPA name reads
// not-known (0) — exactly the old degenerate path. This proves the fix is real
// and that the change is driven entirely by loading caps (handler side).
func TestBuildContextDegenerateWithoutCaps(t *testing.T) {
	start := calendar.NewDate(2021, 1, 4)
	end := calendar.NewDate(2021, 6, 30)
	stocks := []string{"AAPL"}

	ds := sepaContextDataset(t, stocks, start, end)
	// Intentionally do NOT call SetMarketCaps.
	eval := sepaContextEvaluator(t, ds, stocks, start, end)
	cp := eval.buildContext(start, end, true)
	if cp == nil {
		t.Fatal("buildContext returned nil; expected a context provider")
	}
	asOf := time.Date(start.Year, start.Month, start.Day, 0, 0, 0, 0, time.UTC)
	if _, ok := cp.MarketCapAt("AAPL", asOf); ok {
		t.Fatal("without loaded caps the context must be degenerate (cap not-known) — the pre-fix path")
	}
}

// TestDatasetSetMarketCaps covers the dataset accessor contract: SetMarketCaps
// copies (caller mutations don't leak), MarketCap returns 0 for unknown / no
// caps, and the real value otherwise.
func TestDatasetSetMarketCaps(t *testing.T) {
	ds := NewDatasetFromInstruments(nil)
	if got := ds.MarketCap("AAPL"); got != 0 {
		t.Fatalf("no caps loaded: MarketCap = %v, want 0", got)
	}
	src := map[string]float64{"AAPL": 2.7e12, "MSFT": 2.0e12}
	ds.SetMarketCaps(src)
	// Mutating the source after the set must not affect the dataset (defensive copy).
	src["AAPL"] = -1
	if got := ds.MarketCap("AAPL"); got != 2.7e12 {
		t.Fatalf("AAPL cap = %v, want 2.7e12 (source mutation must not leak)", got)
	}
	if got := ds.MarketCap("MSFT"); got != 2.0e12 {
		t.Fatalf("MSFT cap = %v, want 2.0e12", got)
	}
	if got := ds.MarketCap("UNKNOWN"); got != 0 {
		t.Fatalf("unknown ticker: MarketCap = %v, want 0", got)
	}
}
