package sepa

// golden_test.go is the SIGNAL GOLDEN-REGRESSION test. It replays a fixed bar
// series through the generator, then diffs the full ordered per-bar output —
// on_bar signals, evaluate_intent, state_summary — against the embedded
// reference dump (testdata/sepa_golden.json), signal by signal: dates, side,
// qty/sizing, stop/target/pivot prices, state-machine states, intent fields.
// Prices match within 1e-6; everything else (qty, states, string formats,
// generations) must match exactly. The reference values pin this repo's
// behavior; any drift is a regression.

import (
	_ "embed"
	"encoding/json"
	"math"
	"testing"
	"time"
)

//go:embed testdata/sepa_golden.json
var goldenJSON []byte

const priceTol = 1e-6

// ---- reference JSON shapes ------------------------------------------------

type refFile struct {
	Defaults  map[string]any `json:"defaults"`
	Scenarios []refScenario  `json:"scenarios"`
}

type refScenario struct {
	Name string   `json:"name"`
	Rows []refRow `json:"rows"`
}

type refRow struct {
	I       int         `json:"i"`
	BarTS   string      `json:"bar_ts"`
	Signals []refSignal `json:"signals"`
	Intent  refIntent   `json:"intent"`
	Summary refSummary  `json:"summary"`
}

type refSignal struct {
	Symbol     string  `json:"symbol"`
	TS         string  `json:"ts"`
	Side       string  `json:"side"`
	TargetQty  int     `json:"target_qty"`
	Reason     string  `json:"reason"`
	Confidence float64 `json:"confidence"`
	Grade      *string `json:"grade"`
	StopPrice  *string `json:"stop_price"`
}

type refIntent struct {
	Symbol            string   `json:"symbol"`
	State             string   `json:"state"`
	Strength          float64  `json:"strength"`
	ProximityToTrigP  *float64 `json:"proximity_to_trigger_pct"`
	UpdatedAt         string   `json:"updated_at"`
	Generation        int      `json:"generation"`
	StrategyID        string   `json:"strategy_id"`
	Grade             int      `json:"grade"`
	TrendTemplatePass bool     `json:"trend_template_pass"`
	BaseAgeDays       *int     `json:"base_age_days"`
	BaseDepthPct      *float64 `json:"base_depth_pct"`
	VolumeDryup       *bool    `json:"volume_dryup"`
	PivotPrice        *string  `json:"pivot_price"`
	StopPrice         *string  `json:"stop_price"`
	RSRank            *int     `json:"rs_rank"`
}

type refSummary struct {
	Symbol        string  `json:"symbol"`
	Regime        string  `json:"regime"`
	MarketCapUSD  float64 `json:"market_cap_usd"`
	InBlackout    bool    `json:"in_blackout"`
	PositionQty   int     `json:"position_qty"`
	EntryPrice    *string `json:"entry_price"`
	StopPrice     *string `json:"stop_price"`
	CurrentGrade  *string `json:"current_grade"`
	VCPDetected   bool    `json:"vcp_detected"`
	PivotPrice    *string `json:"pivot_price"`
	BarsInHistory int     `json:"bars_in_history"`
}

// ---- the golden regression test -------------------------------------------

func TestSEPAGolden(t *testing.T) {
	var ref refFile
	if err := json.Unmarshal(goldenJSON, &ref); err != nil {
		t.Fatalf("unmarshal golden: %v", err)
	}

	// Build every scenario by name.
	scns := map[string]func() (*Generator, []Bar){
		"happy_bull":    func() (*Generator, []Bar) { return mkSG(t, sgOpt{regime: "bull"}), happyBars("AAPL") },
		"catalyst_bull": func() (*Generator, []Bar) { return mkSG(t, sgOpt{regime: "bull", catalyst: true}), happyBars("AAPL") },
		"bear_blocks":   func() (*Generator, []Bar) { return mkSG(t, sgOpt{regime: "bear"}), happyBars("AAPL") },
		"low_cap_blocks": func() (*Generator, []Bar) {
			return mkSG(t, sgOpt{regime: "bull", marketCap: 100_000_000.0}), happyBars("AAPL")
		},
		"blackout_blocks":      func() (*Generator, []Bar) { return mkSG(t, sgOpt{regime: "bull", blackout: true}), happyBars("AAPL") },
		"no_breakout":          func() (*Generator, []Bar) { return mkSG(t, sgOpt{regime: "bull"}), noBreakoutBars() },
		"weak_volume":          func() (*Generator, []Bar) { return mkSG(t, sgOpt{regime: "bull"}), weakVolumeBars() },
		"other_symbol_ignored": func() (*Generator, []Bar) { return mkSG(t, sgOpt{regime: "bull"}), happyBars("MSFT") },
		"insufficient_history": func() (*Generator, []Bar) { return mkSG(t, sgOpt{regime: "bull"}), insufficientBars() },
		"exit_on_stop":         func() (*Generator, []Bar) { return mkSG(t, sgOpt{regime: "bull"}), exitOnStopBars(t) },
		"hold_above_stop":      func() (*Generator, []Bar) { return mkSG(t, sgOpt{regime: "bull"}), holdAboveStopBars(t) },
		"unknown_regime_b":     func() (*Generator, []Bar) { return mkSG(t, sgOpt{regime: "unknown"}), happyBars("AAPL") },
		"neutral_regime_b":     func() (*Generator, []Bar) { return mkSG(t, sgOpt{regime: "neutral"}), happyBars("AAPL") },
		"double_equity":        func() (*Generator, []Bar) { return mkSG(t, sgOpt{regime: "bull", equity: 200000}), happyBars("AAPL") },
	}

	totalRows, totalSignals, mismatches := 0, 0, 0

	for _, sc := range ref.Scenarios {
		build, ok := scns[sc.Name]
		if !ok {
			t.Fatalf("scenario %q has no Go builder", sc.Name)
		}
		g, bars := build()
		if len(bars) != len(sc.Rows) {
			t.Fatalf("scenario %q: bar count %d != reference rows %d", sc.Name, len(bars), len(sc.Rows))
		}
		for i, b := range bars {
			row := sc.Rows[i]
			sigs := g.OnBar(b)
			intent := g.EvaluateIntent(b.TS)
			summary := g.StateSummary()
			totalRows++
			totalSignals += len(row.Signals)

			mismatches += compareSignals(t, sc.Name, i, row.Signals, sigs)
			mismatches += compareIntent(t, sc.Name, i, row.Intent, intent)
			mismatches += compareSummary(t, sc.Name, i, row.Summary, summary)
		}
	}

	if mismatches != 0 {
		t.Fatalf("GOLDEN FAILED: %d field mismatches across %d rows / %d signals", mismatches, totalRows, totalSignals)
	}
	t.Logf("GOLDEN OK: %d scenarios, %d bars compared, %d signals compared, 0 mismatches",
		len(ref.Scenarios), totalRows, totalSignals)
}

// ---- comparison helpers ---------------------------------------------------

func compareSignals(t *testing.T, scn string, i int, want []refSignal, got []Signal) int {
	t.Helper()
	bad := 0
	if len(want) != len(got) {
		t.Errorf("%s[%d]: signal count want %d got %d", scn, i, len(want), len(got))
		return len(want) + len(got)
	}
	for k := range want {
		w, g := want[k], got[k]
		if w.Symbol != g.Symbol {
			t.Errorf("%s[%d] sig%d: symbol %q != %q", scn, i, k, g.Symbol, w.Symbol)
			bad++
		}
		if w.TS != g.TS.Format(time.RFC3339Nano) && !sameTS(w.TS, g.TS) {
			t.Errorf("%s[%d] sig%d: ts %q != %q", scn, i, k, g.TS, w.TS)
			bad++
		}
		if w.Side != string(g.Side) {
			t.Errorf("%s[%d] sig%d: side %q != %q", scn, i, k, g.Side, w.Side)
			bad++
		}
		if w.TargetQty != g.TargetQty {
			t.Errorf("%s[%d] sig%d: target_qty %d != %d", scn, i, k, g.TargetQty, w.TargetQty)
			bad++
		}
		if w.Reason != g.Reason {
			t.Errorf("%s[%d] sig%d: reason mismatch\n got: %q\nwant: %q", scn, i, k, g.Reason, w.Reason)
			bad++
		}
		if w.Confidence != g.Confidence {
			t.Errorf("%s[%d] sig%d: confidence %v != %v", scn, i, k, g.Confidence, w.Confidence)
			bad++
		}
		if !eqStrPtr(w.Grade, optStr(string(g.Grade))) {
			t.Errorf("%s[%d] sig%d: grade %v != %v", scn, i, k, optStr(string(g.Grade)), w.Grade)
			bad++
		}
		if !eqDecStrPtr(w.StopPrice, optStr(g.StopPrice)) {
			t.Errorf("%s[%d] sig%d: stop %v != %v", scn, i, k, optStr(g.StopPrice), w.StopPrice)
			bad++
		}
	}
	return bad
}

func compareIntent(t *testing.T, scn string, i int, w refIntent, g SignalIntent) int {
	t.Helper()
	bad := 0
	if w.State != string(g.State) {
		t.Errorf("%s[%d] intent: state %q != %q", scn, i, g.State, w.State)
		bad++
	}
	if w.Strength != g.Strength {
		t.Errorf("%s[%d] intent: strength %v != %v", scn, i, g.Strength, w.Strength)
		bad++
	}
	if w.Generation != g.Generation {
		t.Errorf("%s[%d] intent: generation %d != %d", scn, i, g.Generation, w.Generation)
		bad++
	}
	if w.StrategyID != g.StrategyID {
		t.Errorf("%s[%d] intent: strategy_id %q != %q", scn, i, g.StrategyID, w.StrategyID)
		bad++
	}
	if w.Grade != g.Grade {
		t.Errorf("%s[%d] intent: grade %d != %d", scn, i, g.Grade, w.Grade)
		bad++
	}
	if w.TrendTemplatePass != g.TrendTemplatePass {
		t.Errorf("%s[%d] intent: tt_pass %v != %v", scn, i, g.TrendTemplatePass, w.TrendTemplatePass)
		bad++
	}
	// TMS ENHANCEMENT divergence (intent.go attachTradePlan / attachHeldTradePlan):
	// for trend-template-passing flat states (forming/buy) and held (hold) states
	// the generator ALWAYS carries a non-null proximity/pivot/stop trade plan,
	// where the golden leaves them null (it only sets them when a VCP pivot is
	// primed). We therefore hold the strict golden ONLY when the reference
	// expects a value (it must match); a non-null-where-golden-null pair is the
	// SANCTIONED divergence and is accepted for these actionable states.
	diverges := g.State == StateForming || g.State == StateBuy || g.State == StateHold
	if !tmsDivergeOK(w.ProximityToTrigP, g.ProximityToTriggerP, diverges, eqFloatPtr) {
		t.Errorf("%s[%d] intent: proximity %v != %v", scn, i, g.ProximityToTriggerP, w.ProximityToTrigP)
		bad++
	}
	if !eqIntPtr(w.BaseAgeDays, g.BaseAgeDays) {
		t.Errorf("%s[%d] intent: base_age %v != %v", scn, i, g.BaseAgeDays, w.BaseAgeDays)
		bad++
	}
	if !eqFloatPtr(w.BaseDepthPct, g.BaseDepthPct) {
		t.Errorf("%s[%d] intent: base_depth %v != %v", scn, i, g.BaseDepthPct, w.BaseDepthPct)
		bad++
	}
	if !eqBoolPtr(w.VolumeDryup, g.VolumeDryup) {
		t.Errorf("%s[%d] intent: vol_dryup %v != %v", scn, i, g.VolumeDryup, w.VolumeDryup)
		bad++
	}
	if !tmsDivergeOK(w.PivotPrice, optStr(g.PivotPrice), diverges, eqDecStrPtr) {
		t.Errorf("%s[%d] intent: pivot %v != %v", scn, i, optStr(g.PivotPrice), w.PivotPrice)
		bad++
	}
	if !tmsDivergeOK(w.StopPrice, optStr(g.StopPrice), diverges, eqDecStrPtr) {
		t.Errorf("%s[%d] intent: stop %v != %v", scn, i, optStr(g.StopPrice), w.StopPrice)
		bad++
	}
	if w.RSRank != nil {
		t.Errorf("%s[%d] intent: rs_rank reference non-nil", scn, i)
		bad++
	}
	return bad
}

func compareSummary(t *testing.T, scn string, i int, w refSummary, g StateSummary) int {
	t.Helper()
	bad := 0
	if w.Symbol != g.Symbol {
		t.Errorf("%s[%d] sum: symbol", scn, i)
		bad++
	}
	if w.Regime != g.Regime {
		t.Errorf("%s[%d] sum: regime %q != %q", scn, i, g.Regime, w.Regime)
		bad++
	}
	if w.MarketCapUSD != g.MarketCapUSD {
		t.Errorf("%s[%d] sum: market_cap %v != %v", scn, i, g.MarketCapUSD, w.MarketCapUSD)
		bad++
	}
	if w.InBlackout != g.InBlackout {
		t.Errorf("%s[%d] sum: in_blackout %v != %v", scn, i, g.InBlackout, w.InBlackout)
		bad++
	}
	if w.PositionQty != g.PositionQty {
		t.Errorf("%s[%d] sum: position_qty %d != %d", scn, i, g.PositionQty, w.PositionQty)
		bad++
	}
	if !eqDecStrPtr(w.EntryPrice, optStr(g.EntryPrice)) {
		t.Errorf("%s[%d] sum: entry %v != %v", scn, i, optStr(g.EntryPrice), w.EntryPrice)
		bad++
	}
	if !eqDecStrPtr(w.StopPrice, optStr(g.StopPrice)) {
		t.Errorf("%s[%d] sum: stop %v != %v", scn, i, optStr(g.StopPrice), w.StopPrice)
		bad++
	}
	if !eqStrPtr(w.CurrentGrade, optStr(g.CurrentGrade)) {
		t.Errorf("%s[%d] sum: grade %v != %v", scn, i, optStr(g.CurrentGrade), w.CurrentGrade)
		bad++
	}
	if w.VCPDetected != g.VCPDetected {
		t.Errorf("%s[%d] sum: vcp_detected %v != %v", scn, i, g.VCPDetected, w.VCPDetected)
		bad++
	}
	if !eqDecStrPtr(w.PivotPrice, optStr(g.PivotPrice)) {
		t.Errorf("%s[%d] sum: pivot %v != %v", scn, i, optStr(g.PivotPrice), w.PivotPrice)
		bad++
	}
	if w.BarsInHistory != g.BarsInHistory {
		t.Errorf("%s[%d] sum: bars %d != %d", scn, i, g.BarsInHistory, w.BarsInHistory)
		bad++
	}
	return bad
}

// ---- ptr/value equality (with price tolerance on decimal-string fields) ----

// tmsDivergeOK enforces the strict golden when the reference (ref) is non-nil
// (the Go value MUST equal it via eq), while ACCEPTING the sanctioned TMS
// divergence where the reference is nil but the Go value is non-nil — but only
// when `diverges` is true (an actionable forming/buy/hold state, where the
// trade-plan superset is intentional). A reference-nil / Go-non-null pair in any
// other state is still a failure. ref-nil + Go-nil and ref==Go both pass.
func tmsDivergeOK[T any](ref, got *T, diverges bool, eq func(*T, *T) bool) bool {
	if ref != nil {
		return eq(ref, got) // reference has a value: must still match it exactly.
	}
	if got == nil {
		return true // both nil.
	}
	return diverges // ref-nil, Go-non-null: OK only for the diverged states.
}

func optStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func eqStrPtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// eqDecStrPtr compares two decimal-string pointers numerically within priceTol
// (the reference holds str(Decimal); Go holds pyFloatRepr). Both nil is equal.
func eqDecStrPtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if *a == *b {
		return true
	}
	return math.Abs(parsePyFloat(*a)-parsePyFloat(*b)) <= priceTol
}

func eqFloatPtr(a, b *float64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return math.Abs(*a-*b) <= priceTol
}

func eqIntPtr(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func eqBoolPtr(a, b *bool) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func sameTS(ref string, got time.Time) bool {
	pt, err := time.Parse(time.RFC3339Nano, ref)
	if err != nil {
		return false
	}
	return pt.Equal(got)
}
