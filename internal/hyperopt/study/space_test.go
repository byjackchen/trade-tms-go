package study

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/hyperopt/nsga2"
)

func TestSpaceBuilderOrderAndNames(t *testing.T) {
	b, err := NewSpaceBuilder("pairs")
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0)
	for _, p := range b.Space().Params {
		got = append(got, p.Name)
	}
	// pairs searched params in file order: lookback, entry_z, exit_z, capital_per_pair_pct.
	want := []string{"pairs.lookback", "pairs.entry_z", "pairs.exit_z", "pairs.capital_per_pair_pct"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("param order: got %v want %v", got, want)
	}
}

func TestSpaceBuilderJointOrder(t *testing.T) {
	b, err := NewSpaceBuilder("joint")
	if err != nil {
		t.Fatal(err)
	}
	// Joint samples sepa -> sector_rotation -> pairs; every name must be prefixed
	// and the sepa params must precede sector which must precede pairs.
	var lastSub int
	subRank := map[string]int{"sepa": 0, "sector_rotation": 1, "pairs": 2}
	for _, p := range b.Space().Params {
		sub := p.Name[:strings.IndexByte(p.Name, '.')]
		r, ok := subRank[sub]
		if !ok {
			t.Fatalf("unexpected sub-strategy in %q", p.Name)
		}
		if r < lastSub {
			t.Fatalf("joint order violated at %q (rank %d after %d)", p.Name, r, lastSub)
		}
		lastSub = r
	}
}

func TestDecodeAppliesPairsConstraint(t *testing.T) {
	b, err := NewSpaceBuilder("pairs")
	if err != nil {
		t.Fatal(err)
	}
	// Candidate with exit_z (0.9) violating exit_z < entry_z - 0.1 for entry_z=2.0:
	// the constraint clamps exit_z to min(1.0, entry_z-0.1) = min(1.0,1.9)=1.0... but
	// here we pick entry_z low so the clamp bites: entry_z=1.5 -> bound=min(1.0,1.4)=1.0;
	// exit_z=0.9 stays (0.9<1.0). Use exit_z above the bound to force the clamp.
	cand := nsga2.Params{
		"pairs.lookback":             int64(60),
		"pairs.entry_z":              1.5,
		"pairs.exit_z":               2.0, // > bound -> must clamp to 1.0
		"pairs.capital_per_pair_pct": 0.3,
	}
	dec, err := b.Decode(cand)
	if err != nil {
		t.Fatal(err)
	}
	clamped := dec.Overrides["pairs"]["exit_z"]
	if clamped != 1.0 {
		t.Fatalf("exit_z clamp: got %v want 1.0 (min(1.0, entry_z-0.1))", clamped)
	}
	// The RECORDED params keep the raw pre-clamp value (Q5 bug-compatible).
	if dec.RecordedParams["pairs.exit_z"] != 2.0 {
		t.Fatalf("recorded exit_z: got %v want 2.0 (pre-clamp)", dec.RecordedParams["pairs.exit_z"])
	}
}

func TestTuneBaselineShape(t *testing.T) {
	now := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	body, err := TuneBaseline(TuneInput{
		Strategy:    "pairs",
		Tuned:       map[string]float64{"entry_z": 2.7, "lookback": 90},
		StudyName:   "hyperopt-pairs-2026-06-13_12-00-00",
		TrialNumber: 5,
		Now:         now,
	})
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("parse tuned: %v", err)
	}
	params, _ := doc["parameters"].(map[string]any)
	entryZ, _ := params["entry_z"].(map[string]any)
	if entryZ["default"] != 2.7 {
		t.Fatalf("entry_z default: got %v want 2.7", entryZ["default"])
	}
	lookback, _ := params["lookback"].(map[string]any)
	// int param: default must be an integer 90, not 90.0 -> JSON parses to float64 90
	if lookback["default"] != float64(90) {
		t.Fatalf("lookback default: got %v want 90", lookback["default"])
	}
	// metadata rewritten.
	meta, _ := doc["metadata"].(map[string]any)
	if meta["source"] != "tuned" || meta["tuned_from_trial"] != float64(5) {
		t.Fatalf("metadata: %+v", meta)
	}
	// non-tuned params keep their baseline default (exit_z 0.5).
	exitZ, _ := params["exit_z"].(map[string]any)
	if exitZ["default"] != 0.5 {
		t.Fatalf("exit_z default preserved: got %v want 0.5", exitZ["default"])
	}
	// int default must serialize WITHOUT a trailing ".0" (byte-shape check).
	if !strings.Contains(string(body), `"default": 90`) || strings.Contains(string(body), `"default": 90.0`) {
		t.Fatalf("lookback int default surface form wrong:\n%s", body)
	}
}
