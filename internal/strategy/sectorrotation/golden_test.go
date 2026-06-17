package sectorrotation

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// priceTol is the float tolerance for price-derived floats (returns, weights,
// proximity, strength). Per the task acceptance bar: prices within 1e-6,
// everything else exact. We use a much tighter bound (1e-12) because the
// ratioReturn path is deterministic to the last bit on these fixtures; the
// looser 1e-6 is the contractual ceiling.
const priceTol = 1e-9

// --- reference JSON schema ---

type refRoot struct {
	Config struct {
		Universe         []string `json:"universe"`
		MomentumLookback int      `json:"momentum_lookback"`
		TopK             int      `json:"top_k"`
		Equity           float64  `json:"equity"`
		Timezone         string   `json:"timezone"`
	} `json:"config"`
	Start   string       `json:"start"`
	Days    int          `json:"days"`
	Records []refRecord  `json:"records"`
	Final   refStateDict `json:"final_state_dict"`
}

type refRecord struct {
	Day          int             `json:"day"`
	BarDate      string          `json:"bar_date"`
	BarTS        string          `json:"bar_ts"`
	Symbol       string          `json:"symbol"`
	Close        string          `json:"close"`
	Signals      []refSignal     `json:"signals"`
	Intents      []refIntent     `json:"intents"`
	StateSummary refStateSummary `json:"state_summary"`
}

type refSignal struct {
	Symbol    string  `json:"symbol"`
	TS        string  `json:"ts"`
	Side      string  `json:"side"`
	TargetQty int64   `json:"target_qty"`
	Reason    string  `json:"reason"`
	StopPrice *string `json:"stop_price"`
}

type refIntent struct {
	Symbol                string   `json:"symbol"`
	State                 string   `json:"state"`
	Strength              float64  `json:"strength"`
	ProximityToTriggerPct *float64 `json:"proximity_to_trigger_pct"`
	Generation            int64    `json:"generation"`
	StrategyID            string   `json:"strategy_id"`
	MomentumScore         float64  `json:"momentum_score"`
	Rank                  int      `json:"rank"`
	TargetWeight          float64  `json:"target_weight"`
	CurrentWeight         float64  `json:"current_weight"`
}

type refStateSummary struct {
	CurrentHoldings  map[string]int64 `json:"current_holdings"`
	LastUniverseDate *string          `json:"last_universe_date"`
	TopK             int              `json:"top_k"`
	UniverseSize     int              `json:"universe_size"`
}

type refStateDict struct {
	Config struct {
		Universe         []string `json:"universe"`
		MomentumLookback int      `json:"momentum_lookback"`
		TopK             int      `json:"top_k"`
		EquityAtSnapshot float64  `json:"equity_at_snapshot"`
		Timezone         string   `json:"timezone"`
	} `json:"config"`
	History          map[string][]string `json:"history"`
	LastClose        map[string]string   `json:"last_close"`
	LastUniverseDate *string             `json:"last_universe_date"`
	CurrentPositions map[string]int64    `json:"current_positions"`
}

func loadRef(t *testing.T) refRoot {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "golden.json"))
	if err != nil {
		t.Fatalf("read golden.json: %v", err)
	}
	var r refRoot
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("decode golden.json: %v", err)
	}
	return r
}

func floatEq(a, b float64) bool {
	if math.IsNaN(a) && math.IsNaN(b) {
		return true
	}
	return math.Abs(a-b) <= priceTol
}

// TestGolden_SectorRotation replays a fixed multi-symbol merged bar stream and
// asserts the SignalGenerator reproduces the FULL ordered sequence of on_bar
// signals, evaluate_intent results, and state_summary snapshots —
// signal-by-signal, plus the final state_dict. The reference values pin this
// repo's behavior; any drift is a regression.
func TestGolden_SectorRotation(t *testing.T) {
	ref := loadRef(t)

	cfg := Config{
		EquityProvider:   func() float64 { return ref.Config.Equity },
		Universe:         ref.Config.Universe,
		MomentumLookback: ref.Config.MomentumLookback,
		TopK:             ref.Config.TopK,
		Timezone:         ref.Config.Timezone,
	}
	sg, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var barsCompared, signalsCompared, intentsCompared, summariesCompared int

	for ri, rec := range ref.Records {
		ts, err := time.Parse(time.RFC3339Nano, rec.BarTS)
		if err != nil {
			t.Fatalf("rec %d: parse ts %q: %v", ri, rec.BarTS, err)
		}
		ts = ts.UTC()
		close, err := domain.ParsePrice(rec.Close)
		if err != nil {
			t.Fatalf("rec %d: parse close %q: %v", ri, rec.Close, err)
		}
		bar := domain.Bar{
			Symbol: rec.Symbol, TS: ts,
			Open: close, High: close, Low: close, Close: close, Volume: 1_000_000,
		}

		gotSignals := sg.OnBar(bar)
		gotIntents := sg.EvaluateSignal(ts)
		gotSummary := sg.StateSummary()
		barsCompared++

		// --- signals ---
		if len(gotSignals) != len(rec.Signals) {
			t.Fatalf("rec %d (%s %s): signal count got %d want %d",
				ri, rec.BarDate, rec.Symbol, len(gotSignals), len(rec.Signals))
		}
		for si, want := range rec.Signals {
			got := gotSignals[si]
			signalsCompared++
			if string(got.Side) != want.Side {
				t.Errorf("rec %d sig %d: side got %q want %q", ri, si, got.Side, want.Side)
			}
			if got.Symbol != want.Symbol {
				t.Errorf("rec %d sig %d: symbol got %q want %q", ri, si, got.Symbol, want.Symbol)
			}
			if int64(got.TargetQty) != want.TargetQty {
				t.Errorf("rec %d sig %d (%s): target_qty got %d want %d",
					ri, si, got.Symbol, got.TargetQty, want.TargetQty)
			}
			if got.Reason != want.Reason {
				t.Errorf("rec %d sig %d (%s): reason\n got %q\nwant %q", ri, si, got.Symbol, got.Reason, want.Reason)
			}
			gotTS := got.TS.UTC().Format(time.RFC3339Nano)
			wantTS, _ := time.Parse(time.RFC3339Nano, want.TS)
			if !got.TS.Equal(wantTS) {
				t.Errorf("rec %d sig %d: ts got %s want %s", ri, si, gotTS, want.TS)
			}
			if (got.StopPrice == nil) != (want.StopPrice == nil) {
				t.Errorf("rec %d sig %d: stop_price nil mismatch got %v want %v", ri, si, got.StopPrice, want.StopPrice)
			}
		}

		// --- intents (one per universe symbol, in universe order) ---
		if len(gotIntents) != len(rec.Intents) {
			t.Fatalf("rec %d: intent count got %d want %d", ri, len(gotIntents), len(rec.Intents))
		}
		for ii, want := range rec.Intents {
			got := gotIntents[ii]
			intentsCompared++
			if got.Symbol != want.Symbol {
				t.Errorf("rec %d intent %d: symbol got %q want %q", ri, ii, got.Symbol, want.Symbol)
			}
			if string(got.State) != want.State {
				t.Errorf("rec %d intent %d (%s): state got %q want %q", ri, ii, got.Symbol, got.State, want.State)
			}
			if got.Rank != want.Rank {
				t.Errorf("rec %d intent %d (%s): rank got %d want %d", ri, ii, got.Symbol, got.Rank, want.Rank)
			}
			if got.Generation != want.Generation {
				t.Errorf("rec %d intent %d (%s): generation got %d want %d", ri, ii, got.Symbol, got.Generation, want.Generation)
			}
			if got.StrategyID != want.StrategyID {
				t.Errorf("rec %d intent %d: strategy_id got %q want %q", ri, ii, got.StrategyID, want.StrategyID)
			}
			if !floatEq(got.Strength, want.Strength) {
				t.Errorf("rec %d intent %d (%s): strength got %v want %v", ri, ii, got.Symbol, got.Strength, want.Strength)
			}
			if !floatEq(got.MomentumScore, want.MomentumScore) {
				t.Errorf("rec %d intent %d (%s): momentum got %v want %v", ri, ii, got.Symbol, got.MomentumScore, want.MomentumScore)
			}
			if !floatEq(got.TargetWeight, want.TargetWeight) {
				t.Errorf("rec %d intent %d (%s): target_weight got %v want %v", ri, ii, got.Symbol, got.TargetWeight, want.TargetWeight)
			}
			if !floatEq(got.CurrentWeight, want.CurrentWeight) {
				t.Errorf("rec %d intent %d (%s): current_weight got %v want %v", ri, ii, got.Symbol, got.CurrentWeight, want.CurrentWeight)
			}
			if (got.ProximityToTriggerPct == nil) != (want.ProximityToTriggerPct == nil) {
				t.Errorf("rec %d intent %d (%s): proximity nil mismatch got %v want %v",
					ri, ii, got.Symbol, got.ProximityToTriggerPct, want.ProximityToTriggerPct)
			} else if got.ProximityToTriggerPct != nil && !floatEq(*got.ProximityToTriggerPct, *want.ProximityToTriggerPct) {
				t.Errorf("rec %d intent %d (%s): proximity got %v want %v",
					ri, ii, got.Symbol, *got.ProximityToTriggerPct, *want.ProximityToTriggerPct)
			}
		}

		// --- state_summary ---
		summariesCompared++
		if gotSummary.TopK != rec.StateSummary.TopK {
			t.Errorf("rec %d: summary top_k got %d want %d", ri, gotSummary.TopK, rec.StateSummary.TopK)
		}
		if gotSummary.UniverseSize != rec.StateSummary.UniverseSize {
			t.Errorf("rec %d: summary universe_size got %d want %d", ri, gotSummary.UniverseSize, rec.StateSummary.UniverseSize)
		}
		if (gotSummary.LastUniverseDate == nil) != (rec.StateSummary.LastUniverseDate == nil) {
			t.Errorf("rec %d: summary last_universe_date nil mismatch", ri)
		} else if gotSummary.LastUniverseDate != nil && *gotSummary.LastUniverseDate != *rec.StateSummary.LastUniverseDate {
			t.Errorf("rec %d: summary last_universe_date got %q want %q",
				ri, *gotSummary.LastUniverseDate, *rec.StateSummary.LastUniverseDate)
		}
		if len(gotSummary.CurrentHoldings) != len(rec.StateSummary.CurrentHoldings) {
			t.Errorf("rec %d: summary holdings count got %d want %d",
				ri, len(gotSummary.CurrentHoldings), len(rec.StateSummary.CurrentHoldings))
		}
		for sym, wq := range rec.StateSummary.CurrentHoldings {
			if gotSummary.CurrentHoldings[sym] != wq {
				t.Errorf("rec %d: summary holding %s got %d want %d", ri, sym, gotSummary.CurrentHoldings[sym], wq)
			}
		}
	}

	// --- final state_dict ---
	gotSD := sg.StateDict()
	wantSD := ref.Final
	if gotSD.Config.EquityAtSnapshot != wantSD.Config.EquityAtSnapshot {
		t.Errorf("state_dict equity got %v want %v", gotSD.Config.EquityAtSnapshot, wantSD.Config.EquityAtSnapshot)
	}
	if gotSD.Config.MomentumLookback != wantSD.Config.MomentumLookback || gotSD.Config.TopK != wantSD.Config.TopK {
		t.Errorf("state_dict config knobs mismatch: %+v vs %+v", gotSD.Config, wantSD.Config)
	}
	if (gotSD.LastUniverseDate == nil) != (wantSD.LastUniverseDate == nil) {
		t.Errorf("state_dict last_universe_date nil mismatch")
	} else if gotSD.LastUniverseDate != nil && *gotSD.LastUniverseDate != *wantSD.LastUniverseDate {
		t.Errorf("state_dict last_universe_date got %q want %q", *gotSD.LastUniverseDate, *wantSD.LastUniverseDate)
	}
	for sym, wantHist := range wantSD.History {
		gotHist := gotSD.History[sym]
		if len(gotHist) != len(wantHist) {
			t.Errorf("state_dict history %s len got %d want %d", sym, len(gotHist), len(wantHist))
			continue
		}
		for i := range wantHist {
			if gotHist[i] != wantHist[i] {
				t.Errorf("state_dict history %s[%d] got %q want %q", sym, i, gotHist[i], wantHist[i])
			}
		}
	}
	for sym, wantLC := range wantSD.LastClose {
		if gotSD.LastClose[sym] != wantLC {
			t.Errorf("state_dict last_close %s got %q want %q", sym, gotSD.LastClose[sym], wantLC)
		}
	}
	for sym, wantQ := range wantSD.CurrentPositions {
		if gotSD.CurrentPositions[sym] != wantQ {
			t.Errorf("state_dict position %s got %d want %d", sym, gotSD.CurrentPositions[sym], wantQ)
		}
	}

	// --- round-trip load_state then re-dump must be identical ---
	sg2, err := New(cfg)
	if err != nil {
		t.Fatalf("New (roundtrip): %v", err)
	}
	if err := sg2.LoadState(gotSD); err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	rtSD := sg2.StateDict()
	if rtb, _ := json.Marshal(rtSD); true {
		ob, _ := json.Marshal(gotSD)
		if string(rtb) != string(ob) {
			t.Errorf("state_dict round-trip mismatch:\n got %s\nwant %s", rtb, ob)
		}
	}

	t.Logf("golden OK: bars=%d signals=%d intents=%d summaries=%d",
		barsCompared, signalsCompared, intentsCompared, summariesCompared)
}
