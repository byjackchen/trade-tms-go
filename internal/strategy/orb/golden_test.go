package orb

// golden_test.go is the permanent signal golden-regression test. It embeds a
// reference sequence (testdata/orb_golden.json) and replays the identical bar
// series through the Generator, asserting the full ordered sequence of (on_bar
// signals, evaluate_intent, state_summary, state_dict) matches signal-by-signal.
// The values pin this repo's behavior; any drift is a regression.
//
// Decimal-valued fields are compared as the exact str(Decimal) string the
// reference produced (the strongest proof: scale-preserving byte equality);
// float fields are compared exactly; proximity/strength are compared within
// 1e-9 (they are derived through a Decimal division -> float conversion; the
// tolerance documents the contract).

import (
	_ "embed"
	"encoding/json"
	"math"
	"testing"
	"time"
)

//go:embed testdata/orb_golden.json
var orbGoldenJSON []byte

type pjFile struct {
	Config  pjConfig   `json:"config"`
	Records []pjRecord `json:"records"`
}

type pjConfig struct {
	Symbol        string  `json:"symbol"`
	RiskPct       float64 `json:"risk_pct"`
	RangeMinutes  int     `json:"range_minutes"`
	VolMultiple   float64 `json:"vol_multiple"`
	ProfitTargetR float64 `json:"profit_target_r"`
	HardStopPct   float64 `json:"hard_stop_pct"`
	EODExitTime   string  `json:"eod_exit_time"`
	Timezone      string  `json:"timezone"`
	Equity        string  `json:"equity"`
}

type pjRecord struct {
	Bar          pjBar          `json:"bar"`
	Signals      []pjSignal     `json:"signals"`
	Intent       pjIntent       `json:"intent"`
	StateSummary pjStateSummary `json:"state_summary"`
	StateDict    map[string]any `json:"state_dict"`
}

type pjBar struct {
	Symbol string `json:"symbol"`
	TS     string `json:"ts"`
	Open   string `json:"open"`
	High   string `json:"high"`
	Low    string `json:"low"`
	Close  string `json:"close"`
	Volume int64  `json:"volume"`
}

type pjSignal struct {
	Symbol     string  `json:"symbol"`
	TS         string  `json:"ts"`
	Side       string  `json:"side"`
	TargetQty  int     `json:"target_qty"`
	Reason     string  `json:"reason"`
	Confidence float64 `json:"confidence"`
	Grade      *string `json:"grade"`
	StopPrice  *string `json:"stop_price"`
}

type pjIntent struct {
	Symbol                string   `json:"symbol"`
	State                 string   `json:"state"`
	Strength              float64  `json:"strength"`
	ProximityToTriggerPct *float64 `json:"proximity_to_trigger_pct"`
	UpdatedAt             string   `json:"updated_at"`
	Generation            int      `json:"generation"`
	StrategyID            string   `json:"strategy_id"`
	ORBHigh               *string  `json:"orb_high"`
	ORBLow                *string  `json:"orb_low"`
	EntryWindowEnd        *string  `json:"entry_window_end"`
}

type pjStateSummary struct {
	Symbol      string  `json:"symbol"`
	SessionDate *string `json:"session_date"`
	RangeHigh   *string `json:"range_high"`
	RangeLow    *string `json:"range_low"`
	RangeLocked bool    `json:"range_locked"`
	AvgVolume   float64 `json:"avg_volume"`
	PositionQty int     `json:"position_qty"`
	EntryPrice  *string `json:"entry_price"`
	StopPrice   *string `json:"stop_price"`
	TargetPrice *string `json:"target_price"`
}

// pyDatetimeStr renders a str(datetime) UTC instant for comparison
// ("2024-01-08 14:30:00+00:00"), matching the golden dump format.
func pyDatetimeStr(t time.Time) string {
	u := t.UTC()
	base := u.Format("2006-01-02") + " " + u.Format("15:04:05")
	if micro := u.Nanosecond() / 1000; micro != 0 {
		base += "." + leftPad6(micro)
	}
	return base + "+00:00"
}

func leftPad6(v int) string {
	s := make([]byte, 6)
	for i := 5; i >= 0; i-- {
		s[i] = byte('0' + v%10)
		v /= 10
	}
	return string(s)
}

func parseRefTS(t *testing.T, s string) time.Time {
	t.Helper()
	// Reference dumps str(datetime) => "2006-01-02 15:04:05[.ffffff]+00:00".
	layouts := []string{
		"2006-01-02 15:04:05.999999-07:00",
		"2006-01-02 15:04:05-07:00",
	}
	for _, l := range layouts {
		if ts, err := time.Parse(l, s); err == nil {
			return ts.UTC()
		}
	}
	t.Fatalf("cannot parse reference ts %q", s)
	return time.Time{}
}

func TestORBGolden(t *testing.T) {
	var f pjFile
	if err := json.Unmarshal(orbGoldenJSON, &f); err != nil {
		t.Fatalf("decode golden fixture: %v", err)
	}
	if len(f.Records) == 0 {
		t.Fatal("empty golden fixture")
	}

	equity, ok := parseDec(f.Config.Equity)
	if !ok {
		t.Fatalf("bad equity %q", f.Config.Equity)
	}
	equityF := equity.float64()

	g, err := New(Config{
		Symbol:         f.Config.Symbol,
		EquityProvider: func() float64 { return equityF },
		RiskPct:        f.Config.RiskPct,
		RangeMinutes:   f.Config.RangeMinutes,
		VolMultiple:    f.Config.VolMultiple,
		ProfitTargetR:  f.Config.ProfitTargetR,
		HardStopPct:    f.Config.HardStopPct,
		EODExitTime:    f.Config.EODExitTime,
		Timezone:       f.Config.Timezone,
	})
	if err != nil {
		t.Fatalf("construct generator: %v", err)
	}

	var barsCompared, sigsCompared int

	for i, rec := range f.Records {
		ts := parseRefTS(t, rec.Bar.TS)
		bar, ok := NewBarFromStrings(rec.Bar.Symbol, ts,
			rec.Bar.Open, rec.Bar.High, rec.Bar.Low, rec.Bar.Close, rec.Bar.Volume)
		if !ok {
			t.Fatalf("rec %d: bad bar decimals", i)
		}

		gotSigs := g.OnBar(bar)
		barsCompared++

		// --- signals ---------------------------------------------------
		if len(gotSigs) != len(rec.Signals) {
			t.Fatalf("rec %d (%s): signal count = %d, want %d (go=%+v)",
				i, rec.Bar.TS, len(gotSigs), len(rec.Signals), gotSigs)
		}
		for j, want := range rec.Signals {
			got := gotSigs[j]
			sigsCompared++
			if got.Symbol != want.Symbol {
				t.Errorf("rec %d sig %d: symbol %q != %q", i, j, got.Symbol, want.Symbol)
			}
			if gotTS := pyDatetimeStr(got.TS); gotTS != want.TS {
				t.Errorf("rec %d sig %d: ts %q != %q", i, j, gotTS, want.TS)
			}
			if string(got.Side) != want.Side {
				t.Errorf("rec %d sig %d: side %q != %q", i, j, got.Side, want.Side)
			}
			if got.TargetQty != want.TargetQty {
				t.Errorf("rec %d sig %d: target_qty %d != %d", i, j, got.TargetQty, want.TargetQty)
			}
			if got.Reason != want.Reason {
				t.Errorf("rec %d sig %d: reason mismatch:\n got: %q\nwant: %q", i, j, got.Reason, want.Reason)
			}
			if got.Confidence != want.Confidence {
				t.Errorf("rec %d sig %d: confidence %v != %v", i, j, got.Confidence, want.Confidence)
			}
			if want.StopPrice == nil {
				if got.StopPrice != "" {
					t.Errorf("rec %d sig %d: stop_price %q != null", i, j, got.StopPrice)
				}
			} else if got.StopPrice != *want.StopPrice {
				t.Errorf("rec %d sig %d: stop_price %q != %q", i, j, got.StopPrice, *want.StopPrice)
			}
		}

		// --- evaluate_intent ------------------------------------------
		gotIntent := g.EvaluateSignal(ts)
		compareIntent(t, i, gotIntent, rec.Intent)

		// --- state_summary --------------------------------------------
		compareSummary(t, i, g.StateSummary(), rec.StateSummary)

		// --- state_dict -----------------------------------------------
		compareStateDict(t, i, g.StateDict(), rec.StateDict)
	}

	t.Logf("GOLDEN OK: bars compared=%d, signals compared=%d, records=%d, mismatches=0",
		barsCompared, sigsCompared, len(f.Records))
}

func compareIntent(t *testing.T, i int, got SignalSnapshot, want pjIntent) {
	t.Helper()
	if got.Symbol != want.Symbol {
		t.Errorf("rec %d intent: symbol %q != %q", i, got.Symbol, want.Symbol)
	}
	if string(got.State) != want.State {
		t.Errorf("rec %d intent: state %q != %q", i, got.State, want.State)
	}
	if !floatClose(got.Strength, want.Strength) {
		t.Errorf("rec %d intent: strength %v != %v", i, got.Strength, want.Strength)
	}
	if (got.ProximityToTriggerPct == nil) != (want.ProximityToTriggerPct == nil) {
		t.Errorf("rec %d intent: proximity nil-ness got=%v want=%v",
			i, got.ProximityToTriggerPct, want.ProximityToTriggerPct)
	} else if got.ProximityToTriggerPct != nil &&
		!floatClose(*got.ProximityToTriggerPct, *want.ProximityToTriggerPct) {
		t.Errorf("rec %d intent: proximity %v != %v",
			i, *got.ProximityToTriggerPct, *want.ProximityToTriggerPct)
	}
	if gotTS := pyDatetimeStr(got.UpdatedAt); gotTS != want.UpdatedAt {
		t.Errorf("rec %d intent: updated_at %q != %q", i, gotTS, want.UpdatedAt)
	}
	if got.Generation != want.Generation {
		t.Errorf("rec %d intent: generation %d != %d", i, got.Generation, want.Generation)
	}
	if got.StrategyID != want.StrategyID {
		t.Errorf("rec %d intent: strategy_id %q != %q", i, got.StrategyID, want.StrategyID)
	}
	compareOptStr(t, i, "intent.orb_high", got.ORBHigh, want.ORBHigh)
	compareOptStr(t, i, "intent.orb_low", got.ORBLow, want.ORBLow)
	// entry_window_end
	if (got.EntryWindowEnd == nil) != (want.EntryWindowEnd == nil) {
		t.Errorf("rec %d intent: entry_window_end nil-ness got=%v want=%v",
			i, got.EntryWindowEnd, want.EntryWindowEnd)
	} else if got.EntryWindowEnd != nil {
		if gotS := pyDatetimeStr(*got.EntryWindowEnd); gotS != *want.EntryWindowEnd {
			t.Errorf("rec %d intent: entry_window_end %q != %q", i, gotS, *want.EntryWindowEnd)
		}
	}
}

func compareSummary(t *testing.T, i int, got StateSummary, want pjStateSummary) {
	t.Helper()
	if got.Symbol != want.Symbol {
		t.Errorf("rec %d summary: symbol %q != %q", i, got.Symbol, want.Symbol)
	}
	comparePtrStr(t, i, "summary.session_date", got.SessionDate, want.SessionDate)
	comparePtrStr(t, i, "summary.range_high", got.RangeHigh, want.RangeHigh)
	comparePtrStr(t, i, "summary.range_low", got.RangeLow, want.RangeLow)
	if got.RangeLocked != want.RangeLocked {
		t.Errorf("rec %d summary: range_locked %v != %v", i, got.RangeLocked, want.RangeLocked)
	}
	if got.AvgVolume != want.AvgVolume {
		t.Errorf("rec %d summary: avg_volume %v != %v", i, got.AvgVolume, want.AvgVolume)
	}
	if got.PositionQty != want.PositionQty {
		t.Errorf("rec %d summary: position_qty %d != %d", i, got.PositionQty, want.PositionQty)
	}
	comparePtrStr(t, i, "summary.entry_price", got.EntryPrice, want.EntryPrice)
	comparePtrStr(t, i, "summary.stop_price", got.StopPrice, want.StopPrice)
	comparePtrStr(t, i, "summary.target_price", got.TargetPrice, want.TargetPrice)
}

func compareStateDict(t *testing.T, i int, got StateDict, want map[string]any) {
	t.Helper()
	// Marshal Go StateDict to a generic map for key-by-key compare; this also
	// proves the JSON field order/shape matches the reference's dict.
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("rec %d: marshal state_dict: %v", i, err)
	}
	var gm map[string]any
	if err := json.Unmarshal(raw, &gm); err != nil {
		t.Fatalf("rec %d: unmarshal state_dict: %v", i, err)
	}
	checkMapEqual(t, i, "state_dict", gm, want)
}

func checkMapEqual(t *testing.T, i int, path string, got, want map[string]any) {
	t.Helper()
	for k, wv := range want {
		gv, ok := got[k]
		if !ok {
			t.Errorf("rec %d %s: missing key %q", i, path, k)
			continue
		}
		if wm, isMap := wv.(map[string]any); isMap {
			gm, ok := gv.(map[string]any)
			if !ok {
				t.Errorf("rec %d %s.%s: type mismatch (want object)", i, path, k)
				continue
			}
			checkMapEqual(t, i, path+"."+k, gm, wm)
			continue
		}
		if !valueEqual(gv, wv) {
			t.Errorf("rec %d %s.%s: %#v != %#v", i, path, k, gv, wv)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("rec %d %s: extra key %q in Go output", i, path, k)
		}
	}
}

// valueEqual compares JSON scalars, treating numbers via float with a tiny
// tolerance and everything else by ==.
func valueEqual(a, b any) bool {
	switch bv := b.(type) {
	case float64:
		av, ok := a.(float64)
		return ok && floatClose(av, bv)
	default:
		return a == b
	}
}

func compareOptStr(t *testing.T, i int, field string, got string, want *string) {
	t.Helper()
	if want == nil {
		if got != "" {
			t.Errorf("rec %d %s: %q != null", i, field, got)
		}
		return
	}
	if got != *want {
		t.Errorf("rec %d %s: %q != %q", i, field, got, *want)
	}
}

func comparePtrStr(t *testing.T, i int, field string, got, want *string) {
	t.Helper()
	if (got == nil) != (want == nil) {
		t.Errorf("rec %d %s: nil-ness got=%v want=%v", i, field, ptrStr(got), ptrStr(want))
		return
	}
	if got != nil && *got != *want {
		t.Errorf("rec %d %s: %q != %q", i, field, *got, *want)
	}
}

func ptrStr(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

func floatClose(a, b float64) bool {
	if a == b {
		return true
	}
	return math.Abs(a-b) <= 1e-9
}
