package portfolio

import (
	"encoding/json"
	"math"
	"os"
	"testing"
	"time"
)

func mustDate(s string) time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return t.UTC()
}

// TestContextParity replays golden outputs of compute_regime /
// load_sf1_market_caps / load_earnings_calendar captured from the Python
// reference across edge cases (warmup, NaN poisoning, as_of look-ahead,
// dimension filter, datekey ties, blackout boundaries).
func TestContextParity(t *testing.T) {
	raw, err := os.ReadFile("testdata/context_parity.json")
	if err != nil {
		t.Skipf("context parity fixture missing (%v)", err)
	}
	var fixture struct {
		Regime []struct {
			Name   string     `json:"name"`
			Closes []*float64 `json:"closes"`
			Dates  []string   `json:"dates"`
			AsOf   string     `json:"as_of"`
			Want   string     `json:"want"`
		} `json:"regime"`
		SF1 []struct {
			Name      string                   `json:"name"`
			Rows      []map[string]interface{} `json:"rows"`
			AsOf      string                   `json:"as_of"`
			Dimension string                   `json:"dimension"`
			Want      map[string]string        `json:"want"`
		} `json:"sf1"`
		Earnings []struct {
			Name         string                   `json:"name"`
			Rows         []map[string]interface{} `json:"rows"`
			AsOf         string                   `json:"as_of"`
			BlackoutDays int                      `json:"blackout_days"`
			DateColumn   string                   `json:"date_column"`
			Want         map[string]bool          `json:"want"`
		} `json:"earnings"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// --- regime ---
	for _, c := range fixture.Regime {
		t.Run("regime/"+c.Name, func(t *testing.T) {
			bars := make([]SPYBar, len(c.Closes))
			for i, cl := range c.Closes {
				v := math.NaN()
				if cl != nil {
					v = *cl
				}
				var d time.Time
				if c.Dates != nil {
					d = mustDate(c.Dates[i][:10])
				} else {
					// no date column: synthesize sequential dates so as_of (when
					// supplied) is irrelevant; these cases pass as_of="".
					d = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, i)
				}
				bars[i] = SPYBar{Date: d, Close: v}
			}
			var asOf time.Time
			if c.AsOf != "" {
				asOf = mustDate(c.AsOf)
			}
			got := ComputeRegime(bars, asOf)
			if got != c.Want {
				t.Fatalf("got %q want %q", got, c.Want)
			}
		})
	}

	// --- sf1 ---
	for _, c := range fixture.SF1 {
		t.Run("sf1/"+c.Name, func(t *testing.T) {
			rows := make([]SF1Row, 0, len(c.Rows))
			for _, r := range c.Rows {
				row := SF1Row{
					Ticker:  r["ticker"].(string),
					DateKey: mustDate(r["datekey"].(string)),
				}
				if mc, ok := r["marketcap"]; ok && mc != nil {
					row.MarketCap = mc.(float64)
					row.HasMarketCap = true
				}
				if dim, ok := r["dimension"]; ok && dim != nil {
					row.Dimension = dim.(string)
					row.HasDimension = true
				}
				rows = append(rows, row)
			}
			got := LoadSF1MarketCaps(rows, mustDate(c.AsOf), c.Dimension)
			if len(got) != len(c.Want) {
				t.Fatalf("len got %d want %d (%v)", len(got), len(c.Want), c.Want)
			}
			for tk, wantStr := range c.Want {
				g, ok := got[tk]
				if !ok {
					t.Fatalf("ticker %s missing", tk)
				}
				if g.Cmp(MustDec(wantStr)) != 0 {
					t.Fatalf("%s: got %s want %s", tk, g.r.FloatString(4), wantStr)
				}
			}
		})
	}

	// --- earnings ---
	for _, c := range fixture.Earnings {
		t.Run("earnings/"+c.Name, func(t *testing.T) {
			rows := make([]EarningsRow, 0, len(c.Rows))
			for _, r := range c.Rows {
				tk, hasTk := r["ticker"]
				dateVal, hasDate := r[c.DateColumn]
				if !hasTk || !hasDate {
					// mirror Python: a frame missing ticker or the date column
					// yields {} — represent by emitting no rows so the loader
					// returns empty (the loader itself can't know the column was
					// named differently; the missing-column case is exercised by
					// the empty Want).
					continue
				}
				rows = append(rows, EarningsRow{
					Ticker:     tk.(string),
					ReportDate: mustDate(dateVal.(string)),
				})
			}
			got := LoadEarningsCalendar(rows, mustDate(c.AsOf), c.BlackoutDays)
			if len(got) != len(c.Want) {
				t.Fatalf("len got %d want %d (got=%v want=%v)", len(got), len(c.Want), got, c.Want)
			}
			for tk, wv := range c.Want {
				if got[tk] != wv {
					t.Fatalf("%s: got %v want %v", tk, got[tk], wv)
				}
			}
		})
	}
}

// TestComputeRegimeHandDerived re-derives the warmup / slope / NaN edges by hand.
func TestComputeRegimeHandDerived(t *testing.T) {
	bar := func(i int, c float64) SPYBar {
		return SPYBar{Date: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, i), Close: c}
	}
	mk := func(n int, f func(i int) float64) []SPYBar {
		out := make([]SPYBar, n)
		for i := 0; i < n; i++ {
			out[i] = bar(i, f(i))
		}
		return out
	}

	// < 200 bars -> neutral.
	if got := ComputeRegime(mk(199, func(i int) float64 { return 100 }), time.Time{}); got != RegimeNeutral {
		t.Fatalf("199 bars -> %q want neutral", got)
	}
	// exactly 200 rising -> ma_then (index 169) NaN -> warning.
	if got := ComputeRegime(mk(200, func(i int) float64 { return float64(100 + i) }), time.Time{}); got != RegimeWarning {
		t.Fatalf("200 rising -> %q want warning", got)
	}
	// 231 rising -> ma_then valid, slope > 0 -> bull.
	if got := ComputeRegime(mk(231, func(i int) float64 { return float64(100 + i) }), time.Time{}); got != RegimeBull {
		t.Fatalf("231 rising -> %q want bull", got)
	}
	// flat plateau -> close==MA, slope 0 -> warning.
	if got := ComputeRegime(mk(250, func(i int) float64 { return 200 }), time.Time{}); got != RegimeWarning {
		t.Fatalf("flat -> %q want warning", got)
	}
	// falling -> close < MA -> bear.
	if got := ComputeRegime(mk(250, func(i int) float64 { return float64(500 - i) }), time.Time{}); got != RegimeBear {
		t.Fatalf("falling -> %q want bear", got)
	}
	// NaN in the last-200 window poisons last_ma -> neutral.
	poisoned := mk(250, func(i int) float64 { return float64(100 + i) })
	poisoned[245].Close = math.NaN()
	if got := ComputeRegime(poisoned, time.Time{}); got != RegimeNeutral {
		t.Fatalf("NaN-in-window -> %q want neutral", got)
	}
}

// TestEarningsBlackoutBoundaries re-derives the ±N inclusive window by hand.
func TestEarningsBlackoutBoundaries(t *testing.T) {
	asOf := mustDate("2024-06-10")
	rows := func(d string) []EarningsRow {
		return []EarningsRow{{Ticker: "X", ReportDate: mustDate(d)}}
	}
	// exactly 5 days before -> in.
	if !LoadEarningsCalendar(rows("2024-06-05"), asOf, 5)["X"] {
		t.Fatal("exactly -5 days must be in blackout (inclusive)")
	}
	// exactly 5 days after -> in.
	if !LoadEarningsCalendar(rows("2024-06-15"), asOf, 5)["X"] {
		t.Fatal("exactly +5 days must be in blackout (inclusive)")
	}
	// 6 days before -> out.
	if LoadEarningsCalendar(rows("2024-06-04"), asOf, 5)["X"] {
		t.Fatal("6 days before must be out for N=5")
	}
	// absence == false: a non-blackout ticker is ABSENT, not false.
	out := LoadEarningsCalendar(rows("2024-01-01"), asOf, 5)
	if _, present := out["X"]; present {
		t.Fatal("non-blackout ticker must be ABSENT from the map")
	}
}
