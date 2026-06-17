package hyperopt

import (
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"
)

// fracTrial is a deterministic search-space trial: suggest_float returns
// (low+high)/2 at the midpoint, suggest_int returns (low+high)//2 (floor div).
type fracTrial struct {
	frac   float64
	mid    bool // true => float midpoint, int floor-mid
	params map[string]float64
}

func (f *fracTrial) record(name string, v float64) {
	if f.params == nil {
		f.params = map[string]float64{}
	}
	f.params[name] = v
}

func (f *fracTrial) SuggestFloat(name string, low, high float64) float64 {
	var v float64
	if f.mid {
		v = (low + high) / 2.0
	} else {
		// float64(...) around the product forces a rounding step so Go does
		// not fuse this into an FMA, keeping the arithmetic bit-identical
		// across platforms (arm64 vs x86).
		v = low + float64((high-low)*f.frac)
	}
	f.record(name, v)
	return v
}

func (f *fracTrial) SuggestInt(name string, low, high int64) int64 {
	var v int64
	if f.mid {
		v = (low + high) / 2 // floor for non-negative
	} else {
		v = int64(float64(low) + float64(high-low)*f.frac)
	}
	f.record(name, float64(v))
	return v
}

func TestSuggestWithGolden(t *testing.T) {
	raw, err := os.ReadFile("testdata/loader_golden.json")
	if err != nil {
		t.Skipf("fixture missing (%v)", err)
	}
	var fix struct {
		Suggest map[string]struct {
			Sampled map[string]float64 `json:"sampled"`
		} `json:"suggest"`
	}
	if err := json.Unmarshal(raw, &fix); err != nil {
		t.Fatal(err)
	}
	check := func(t *testing.T, strat string, trial *fracTrial, want map[string]float64) {
		sp, err := LoadBaselineParams(strat)
		if err != nil {
			t.Fatal(err)
		}
		got, err := SuggestWith(sp, trial)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(want) {
			t.Fatalf("%s: got %d keys, want %d (%v vs %v)", strat, len(got), len(want), got, want)
		}
		for k, w := range want {
			if g, ok := got[k]; !ok || g != w {
				t.Fatalf("%s.%s: got %v (ok=%v) want %v", strat, k, g, ok, w)
			}
		}
	}
	// midpoint trials for the three registered strategies
	for _, strat := range SearchSpaceStrategies {
		check(t, strat, &fracTrial{mid: true}, fix.Suggest[strat].Sampled)
	}
	// fractional pairs trials (exercise the exit_z clamp)
	for _, frac := range []float64{0.0, 0.25, 0.9, 1.0} {
		key := "pairs_frac_" + strings.TrimRight(strings.TrimRight(jsonFloat(frac), "0"), ".")
		// match the fixture key naming (float str form)
		key = "pairs_frac_" + pyFloatStr(frac)
		want, ok := fix.Suggest[key]
		if !ok {
			t.Fatalf("missing fixture key %q", key)
		}
		check(t, "pairs", &fracTrial{frac: frac}, want.Sampled)
	}
}

func jsonFloat(f float64) string { b, _ := json.Marshal(f); return string(b) }

// pyFloatStr renders a float in the str(float) surface form for the fractions
// used in the fixture keys (0.0, 0.25, 0.9, 1.0).
func pyFloatStr(f float64) string {
	switch f {
	case 0.0:
		return "0.0"
	case 1.0:
		return "1.0"
	case 0.25:
		return "0.25"
	case 0.9:
		return "0.9"
	}
	return jsonFloat(f)
}

func TestSafeEvalGolden(t *testing.T) {
	raw, err := os.ReadFile("testdata/loader_golden.json")
	if err != nil {
		t.Skipf("fixture missing (%v)", err)
	}
	var fix struct {
		SafeEval map[string]float64 `json:"safe_eval"`
	}
	if err := json.Unmarshal(raw, &fix); err != nil {
		t.Fatal(err)
	}
	// Each expression's scope must mirror the generator.
	scopes := map[string]map[string]float64{
		"min(1.0, entry_z - 0.1)": {"entry_z": 2.0},
		"max(0.1, exit_z)":        {"exit_z": 0.05},
		"abs(-3.5)":               {},
		"1+2*3-4/2":               {},
		"-x":                      {"x": 5.0},
		"min(a,b,c)":              {"a": 3.0, "b": 1.0, "c": 2.0},
		"(entry_z - 0.1)":         {"entry_z": 2.5},
	}
	for expr, want := range fix.SafeEval {
		got, err := safeEval(expr, scopes[expr])
		if err != nil {
			t.Fatalf("%q: unexpected error %v", expr, err)
		}
		if math.Abs(got-want) > 1e-12 {
			t.Fatalf("%q: got %v want %v", expr, got, want)
		}
	}
}

func TestSafeEvalErrors(t *testing.T) {
	// undefined variable -> exact reference message.
	if _, err := safeEval("foo + 1", map[string]float64{}); err == nil || err.Error() != "undefined: foo" {
		t.Fatalf("undefined: got %v", err)
	}
	// unknown function -> "unsupported function" prefix (only the prefix is
	// spec'd).
	if _, err := safeEval("pow(2,3)", map[string]float64{}); err == nil || !strings.HasPrefix(err.Error(), "unsupported function") {
		t.Fatalf("unknown func: got %v", err)
	}
	// division by zero.
	if _, err := safeEval("1/0", map[string]float64{}); err == nil || err.Error() != "division by zero" {
		t.Fatalf("div0: got %v", err)
	}
	// keyword args are not expressible in this grammar; ensure a malformed call
	// errors rather than silently succeeding.
	if _, err := safeEval("min(1,)", map[string]float64{}); err == nil {
		t.Fatal("trailing-comma/empty arg must error")
	}
}

func TestLoaderValidation(t *testing.T) {
	cases := []struct {
		raw      string
		strategy string
		wantErr  string
	}{
		{`{"schema_version":1,"parameters":{}}`, "sepa", "missing required field: strategy"},
		{`{"strategy":"pairs","schema_version":1}`, "sepa", "file declared strategy 'pairs' but loader was asked for 'sepa'"},
		{`{"strategy":"sepa","schema_version":2}`, "sepa", "unsupported schema_version 2"},
		{`{"strategy":"sepa","schema_version":1,"parameters":{"x":{"default":1,"type":"weird"}}}`, "sepa", "parameter 'x': type 'weird' not in {'float', 'int', 'list', 'str'}"},
		{`{"strategy":"sepa","schema_version":1,"parameters":{"x":{"default":"a","type":"str","search":{"low":1,"high":2}}}}`, "sepa", "parameter 'x': search not supported on type 'str'"},
		{`{"strategy":"sepa","schema_version":1,"constraints":[{"kind":"bad","param":"x","expression":"1"}]}`, "sepa", "constraint kind 'bad' not in {'clamp_high', 'clamp_low'}"},
	}
	for _, c := range cases {
		_, err := ParseStrategyParams([]byte(c.raw), c.strategy)
		if err == nil || err.Error() != c.wantErr {
			t.Fatalf("raw=%s: got err %v, want %q", c.raw, err, c.wantErr)
		}
	}
}

func TestDefaultsDictAndOrder(t *testing.T) {
	sp, err := LoadBaselineParams("sepa")
	if err != nil {
		t.Fatal(err)
	}
	// File order preserved.
	wantOrder := []string{"risk_pct", "market_cap_min_usd", "hard_stop_pct", "pivot_buffer_pct", "breakout_volume_multiple", "vcp_lookback", "history_max_bars", "timezone"}
	if len(sp.Parameters) != len(wantOrder) {
		t.Fatalf("param count %d want %d", len(sp.Parameters), len(wantOrder))
	}
	for i, w := range wantOrder {
		if sp.Parameters[i].Name != w {
			t.Fatalf("param %d = %q want %q (file order not preserved)", i, sp.Parameters[i].Name, w)
		}
	}
	// defaults_dict includes non-search params.
	d, err := DefaultsDict(sp)
	if err != nil {
		t.Fatal(err)
	}
	if d["timezone"] != "America/New_York" {
		t.Fatalf("timezone default %v", d["timezone"])
	}
	if d["history_max_bars"].(float64) != 1000 {
		t.Fatalf("history_max_bars default %v", d["history_max_bars"])
	}
	// suggest excludes non-search params (history_max_bars, timezone).
	sampled, err := SuggestWith(sp, &fracTrial{mid: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := sampled["history_max_bars"]; ok {
		t.Fatal("history_max_bars must NOT be sampled")
	}
	if _, ok := sampled["timezone"]; ok {
		t.Fatal("timezone must NOT be sampled")
	}
}

func TestRegistry(t *testing.T) {
	if len(SearchSpaceStrategies) != 3 {
		t.Fatalf("want 3 registered strategies, got %v", SearchSpaceStrategies)
	}
	if _, err := SuggestParams("intraday_breakout", &fracTrial{mid: true}); err == nil || err.Error() != "unknown strategy: intraday_breakout" {
		t.Fatalf("intraday_breakout must be unregistered: %v", err)
	}
	joint, err := SuggestJointParams(&fracTrial{mid: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range SearchSpaceStrategies {
		if _, ok := joint[s]; !ok {
			t.Fatalf("joint missing %s", s)
		}
	}
}
