package pairs

// golden_test.go is the SIGNAL GOLDEN-REGRESSION test for the Pairs strategy. It
// replays a fixed bar series through the generator, then diffs the full ordered
// per-bar output — on_bar() signals, evaluate_intent(), state_summary() — and
// the final state_dict() against the embedded reference
// (testdata/pairs_golden.json), signal by signal: dates, side, qty/sizing,
// z-score, hedge ratio, reason strings (incl the Greek β and +.2f/.3f formats),
// state-machine states, and intent fields. Prices/floats match within priceTol;
// everything else (qty, states, generations, strings) matches exactly. The
// reference values pin this repo's behavior; any drift is a regression.
//
// The fixture is a representative COVID-dislocation window (2020) over the 3
// default pairs (KO/PEP, MA/V, XOM/CVX) across 3 parameter scenarios
// (baseline, aggressive, exit_zero) — exercising entries in both spread
// directions, mean-reversion closes, divergence (loss-cap) closes, and
// re-entries.

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/domain"
)

//go:embed testdata/pairs_golden.json
var goldenJSON []byte

const priceTol = 1e-6

// ---- reference JSON shapes (separators-compact dump) ----------------------

type gBar struct {
	Symbol string `json:"symbol"`
	TS     string `json:"ts"`
	Close  string `json:"close"`
}
type gSignal struct {
	Symbol     string  `json:"symbol"`
	TS         string  `json:"ts"`
	Side       string  `json:"side"`
	TargetQty  int64   `json:"target_qty"`
	Reason     string  `json:"reason"`
	Confidence float64 `json:"confidence"`
}
type gIntent struct {
	Symbol     string   `json:"symbol"`
	State      string   `json:"state"`
	Strength   float64  `json:"strength"`
	Proximity  *float64 `json:"proximity_to_trigger_pct"`
	Generation int64    `json:"generation"`
	StrategyID string   `json:"strategy_id"`
	PairID     string   `json:"pair_id"`
	LegRole    string   `json:"leg_role"`
	ZScore     float64  `json:"z_score"`
	ZEntry     float64  `json:"z_entry_threshold"`
	ZExit      float64  `json:"z_exit_threshold"`
	Hedge      float64  `json:"hedge_ratio"`
}
type gStep struct {
	Bar     gBar            `json:"bar"`
	Signals []gSignal       `json:"signals"`
	Intents []gIntent       `json:"intents"`
	Summary json.RawMessage `json:"state_summary"`
}
type gScenario struct {
	Name   string `json:"name"`
	Config struct {
		Pairs             [][]string `json:"pairs"`
		Lookback          int        `json:"lookback"`
		EntryZ            float64    `json:"entry_z"`
		ExitZ             float64    `json:"exit_z"`
		CapitalPerPairPct float64    `json:"capital_per_pair_pct"`
		Timezone          string     `json:"timezone"`
		Equity            float64    `json:"equity"`
	} `json:"config"`
	Steps     []gStep         `json:"steps"`
	StateDict json.RawMessage `json:"state_dict"`
}
type gDump struct {
	Tickers   []string    `json:"tickers"`
	NBars     int         `json:"n_bars"`
	Scenarios []gScenario `json:"scenarios"`
}

func gParseTS(t *testing.T, s string) time.Time {
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse ts %q: %v", s, err)
	}
	return ts.UTC()
}

// TestGolden is the locked, embedded signal golden-regression proof.
func TestGolden(t *testing.T) {
	var d gDump
	if err := json.Unmarshal(goldenJSON, &d); err != nil {
		t.Fatalf("unmarshal golden: %v", err)
	}
	if len(d.Scenarios) == 0 {
		t.Fatal("golden has no scenarios")
	}

	var bars, sigs, intents, summaries int
	for _, sc := range d.Scenarios {
		pairs := make([]Pair, 0, len(sc.Config.Pairs))
		for _, p := range sc.Config.Pairs {
			pairs = append(pairs, Pair{LongLeg: p[0], ShortLeg: p[1]})
		}
		gen, err := New(Config{
			EquityProvider:    ConstantEquity(sc.Config.Equity),
			Pairs:             pairs,
			Lookback:          sc.Config.Lookback,
			EntryZ:            sc.Config.EntryZ,
			ExitZ:             sc.Config.ExitZ,
			CapitalPerPairPct: sc.Config.CapitalPerPairPct,
			Timezone:          sc.Config.Timezone,
		})
		if err != nil {
			t.Fatalf("scenario %s: New: %v", sc.Name, err)
		}
		for si, step := range sc.Steps {
			ts := gParseTS(t, step.Bar.TS)
			price, err := domain.ParsePrice(step.Bar.Close)
			if err != nil {
				t.Fatalf("parse price %q: %v", step.Bar.Close, err)
			}
			bar := domain.Bar{Symbol: step.Bar.Symbol, TS: ts, Open: price, High: price, Low: price, Close: price, Volume: 1}
			gotSig := gen.OnBar(bar, step.Bar.Close)
			gotInt := gen.EvaluateSignal(ts)
			bars++

			// signals
			if len(gotSig) != len(step.Signals) {
				t.Fatalf("[%s step %d %s] signal count got %d want %d", sc.Name, si, step.Bar.Symbol, len(gotSig), len(step.Signals))
			}
			for k := range gotSig {
				sigs++
				g, w := gotSig[k], step.Signals[k]
				if g.Symbol != w.Symbol || string(g.Side) != w.Side ||
					int64(g.TargetQty) != w.TargetQty || g.Reason != w.Reason ||
					!gFloatEq(g.Confidence, w.Confidence) || !g.TS.Equal(gParseTS(t, w.TS)) {
					t.Fatalf("[%s step %d] signal %d:\n got=%+v\nwant=%+v", sc.Name, si, k, g, w)
				}
			}

			// intents
			if len(gotInt) != len(step.Intents) {
				t.Fatalf("[%s step %d] intent count got %d want %d", sc.Name, si, len(gotInt), len(step.Intents))
			}
			for k := range gotInt {
				intents++
				g, w := gotInt[k], step.Intents[k]
				if g.Symbol != w.Symbol || string(g.State) != w.State ||
					!gFloatEq(g.Strength, w.Strength) || !gPtrFloatEq(g.ProximityToTriggerPct, w.Proximity) ||
					g.Generation != w.Generation || g.StrategyID != w.StrategyID ||
					g.PairID != w.PairID || string(g.LegRole) != w.LegRole ||
					!gFloatEq(g.ZScore, w.ZScore) || !gFloatEq(g.ZEntryThreshold, w.ZEntry) ||
					!gFloatEq(g.ZExitThreshold, w.ZExit) || !gFloatEq(g.HedgeRatio, w.Hedge) {
					t.Fatalf("[%s step %d] intent %d:\n got=%+v\nwant=%+v", sc.Name, si, k, g, w)
				}
			}

			// state_summary (semantic deep-compare)
			gotSum, _ := json.Marshal(gen.StateSummary())
			if diff := gJSONDiff(t, gotSum, step.Summary); diff != "" {
				t.Fatalf("[%s step %d] state_summary: %s", sc.Name, si, diff)
			}
			summaries++
		}

		// state_dict at end of scenario
		gotSD, _ := json.Marshal(gen.StateDict())
		if diff := gJSONDiff(t, gotSD, sc.StateDict); diff != "" {
			t.Fatalf("[%s] state_dict: %s", sc.Name, diff)
		}
	}
	t.Logf("GOLDEN OK: scenarios=%d bars=%d signals=%d intents=%d summaries=%d mismatches=0",
		len(d.Scenarios), bars, sigs, intents, summaries)
}

func gFloatEq(a, b float64) bool {
	if math.IsNaN(a) && math.IsNaN(b) {
		return true
	}
	if a == b {
		return true
	}
	return math.Abs(a-b) <= priceTol*math.Max(1, math.Max(math.Abs(a), math.Abs(b)))
}
func gPtrFloatEq(a, b *float64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return gFloatEq(*a, *b)
}

func gJSONDiff(t *testing.T, got, want []byte) string {
	var ga, wa any
	if err := json.Unmarshal(got, &ga); err != nil {
		t.Fatalf("unmarshal got: %v", err)
	}
	if err := json.Unmarshal(want, &wa); err != nil {
		t.Fatalf("unmarshal want: %v", err)
	}
	return gDeep("$", ga, wa)
}

func gDeep(path string, a, b any) string {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			return fmt.Sprintf("%s: object vs %T", path, b)
		}
		if len(av) != len(bv) {
			return fmt.Sprintf("%s: object size %d vs %d", path, len(av), len(bv))
		}
		for k, va := range av {
			vb, ok := bv[k]
			if !ok {
				return fmt.Sprintf("%s.%s: missing in want", path, k)
			}
			if d := gDeep(path+"."+k, va, vb); d != "" {
				return d
			}
		}
		return ""
	case []any:
		bv, ok := b.([]any)
		if !ok {
			return fmt.Sprintf("%s: array vs %T", path, b)
		}
		if len(av) != len(bv) {
			return fmt.Sprintf("%s: array len %d vs %d", path, len(av), len(bv))
		}
		for i := range av {
			if d := gDeep(fmt.Sprintf("%s[%d]", path, i), av[i], bv[i]); d != "" {
				return d
			}
		}
		return ""
	case float64:
		bf, ok := b.(float64)
		if !ok {
			return fmt.Sprintf("%s: number vs %T", path, b)
		}
		if !gFloatEq(av, bf) {
			return fmt.Sprintf("%s: %v vs %v", path, av, bf)
		}
		return ""
	case nil:
		if b != nil {
			return fmt.Sprintf("%s: null vs %v", path, b)
		}
		return ""
	default:
		if a != b {
			return fmt.Sprintf("%s: %v vs %v", path, a, b)
		}
		return ""
	}
}
