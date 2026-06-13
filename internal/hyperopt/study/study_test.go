package study

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/engine"
)

// syntheticPairs builds a deterministic dataset for the three baseline pairs
// (KO/PEP, MA/V, XOM/CVX) over [start-warmup, end] with smooth, mean-reverting
// spreads so a pairs backtest actually trades. Bars are daily, weekdays only.
func syntheticPairs(t *testing.T, start, end calendar.Date) *Dataset {
	t.Helper()
	legs := []string{"KO", "PEP", "MA", "V", "XOM", "CVX"}
	loadStart := start.AddDays(-spyWarmupDays)
	lo := midnight(loadStart)
	hi := midnight(end)

	ibs := make([]engine.InstrumentBars, 0, len(legs))
	for li, sym := range legs {
		var bars []domain.Bar
		day := lo
		i := 0
		base := 100.0 + float64(li)*5.0
		for !day.After(hi) {
			if wd := day.Weekday(); wd == time.Saturday || wd == time.Sunday {
				day = day.AddDate(0, 0, 1)
				continue
			}
			// Oscillating price so spreads mean-revert; pairs trade the spread.
			px := base + 8.0*math.Sin(float64(i)/9.0+float64(li))
			p, err := domain.PriceFromFloat64(px)
			if err != nil {
				t.Fatal(err)
			}
			bars = append(bars, domain.Bar{
				Symbol: sym, TS: day, Open: p, High: p, Low: p, Close: p, Volume: 1_000_000,
			})
			day = day.AddDate(0, 0, 1)
			i++
		}
		ibs = append(ibs, engine.InstrumentBars{Symbol: sym, Bars: bars})
	}
	return NewDatasetFromInstruments(ibs)
}

func pairsConfig(ds *Dataset, start, end calendar.Date) Config {
	return Config{
		Strategy:        "pairs",
		Start:           start,
		End:             end,
		Population:      4,
		Generations:     2,
		Seed:            42,
		Workers:         1,
		WalkForward:     true,
		Folds:           1,
		EmbargoDays:     5,
		StartingBalance: 100000,
		Dataset:         ds,
		RunsDir:         "", // set per test
	}
}

// runStudy runs a study with the given config into a fresh temp dir and returns
// the coordinator + result.
func runStudy(t *testing.T, cfg Config) (*Coordinator, *Result) {
	t.Helper()
	dir := t.TempDir()
	cfg.RunsDir = dir
	if cfg.StudyTS == "" {
		cfg.StudyTS = "2026-01-02_03-04-05"
	}
	c, err := NewCoordinator(cfg, nil)
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	res, err := c.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return c, res
}

// readTrials parses every trial_*.json under the study dir, in number order.
func readTrials(t *testing.T, dir string) []map[string]any {
	t.Helper()
	trialsDir := filepath.Join(dir, "trials")
	entries, err := os.ReadDir(trialsDir)
	if err != nil {
		t.Fatalf("read trials dir: %v", err)
	}
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(trialsDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		out = append(out, m)
	}
	return out
}

func TestStudyDeterministicEndToEnd(t *testing.T) {
	start := calendar.NewDate(2023, 1, 2)
	end := calendar.NewDate(2023, 12, 29)

	ds := syntheticPairs(t, start, end)
	cfg := pairsConfig(ds, start, end)

	c1, res1 := runStudy(t, cfg)
	trials1 := readTrials(t, c1.StudyDir())

	// Second run, same seed, same dataset, fresh dir -> identical trials.
	c2, res2 := runStudy(t, cfg)
	trials2 := readTrials(t, c2.StudyDir())

	if len(trials1) != cfg.Population*cfg.Generations {
		t.Fatalf("trial count: got %d want %d", len(trials1), cfg.Population*cfg.Generations)
	}
	if len(trials1) != len(trials2) {
		t.Fatalf("trial count differs across runs: %d vs %d", len(trials1), len(trials2))
	}
	for i := range trials1 {
		// Compare params + metrics + state (timestamps/duration excluded).
		for _, k := range []string{"number", "strategy", "params", "metrics", "folds", "state"} {
			a, _ := json.Marshal(trials1[i][k])
			b, _ := json.Marshal(trials2[i][k])
			if string(a) != string(b) {
				t.Fatalf("trial %d field %q differs:\n A=%s\n B=%s", i, k, a, b)
			}
		}
	}
	if res1.StudyName != res2.StudyName {
		t.Fatalf("study name differs: %q vs %q", res1.StudyName, res2.StudyName)
	}

	// study.json + progress.json exist and are well-formed.
	for _, name := range []string{"study.json", "progress.json"} {
		b, err := os.ReadFile(filepath.Join(c1.StudyDir(), name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
	}
	// progress status COMPLETE on a finished study.
	pb, _ := os.ReadFile(filepath.Join(c1.StudyDir(), "progress.json"))
	var prog map[string]any
	_ = json.Unmarshal(pb, &prog)
	if prog["status"] != "COMPLETE" {
		t.Fatalf("progress status: got %v want COMPLETE", prog["status"])
	}
}

func TestStudyWalkForwardFoldsRecorded(t *testing.T) {
	start := calendar.NewDate(2023, 1, 2)
	end := calendar.NewDate(2023, 12, 29)
	ds := syntheticPairs(t, start, end)
	cfg := pairsConfig(ds, start, end)
	cfg.Folds = 2
	c, _ := runStudy(t, cfg)
	trials := readTrials(t, c.StudyDir())
	// Every COMPLETE trial must carry 2 folds.
	sawComplete := false
	for _, tr := range trials {
		if tr["state"] != "COMPLETE" {
			continue
		}
		sawComplete = true
		folds, _ := tr["folds"].([]any)
		if len(folds) != 2 {
			t.Fatalf("trial %v: folds=%d want 2", tr["number"], len(folds))
		}
		// Each fold carries the fold index + metric keys.
		f0, _ := folds[0].(map[string]any)
		if _, ok := f0["fold"]; !ok {
			t.Fatalf("fold payload missing 'fold' key: %v", f0)
		}
		if _, ok := f0["sharpe"]; !ok {
			t.Fatalf("fold payload missing 'sharpe' key: %v", f0)
		}
	}
	if !sawComplete {
		t.Fatal("no COMPLETE trial produced")
	}
}

func TestStudySingleWindowNoFolds(t *testing.T) {
	start := calendar.NewDate(2023, 1, 2)
	end := calendar.NewDate(2023, 12, 29)
	ds := syntheticPairs(t, start, end)
	cfg := pairsConfig(ds, start, end)
	cfg.WalkForward = false
	c, _ := runStudy(t, cfg)
	trials := readTrials(t, c.StudyDir())
	for _, tr := range trials {
		if tr["state"] != "COMPLETE" {
			continue
		}
		folds, _ := tr["folds"].([]any)
		if len(folds) != 0 {
			t.Fatalf("single-window trial %v has %d folds, want 0", tr["number"], len(folds))
		}
	}
}

func TestStudyParallelMatchesSequential(t *testing.T) {
	start := calendar.NewDate(2023, 1, 2)
	end := calendar.NewDate(2023, 12, 29)
	ds := syntheticPairs(t, start, end)

	cfgSeq := pairsConfig(ds, start, end)
	cfgSeq.Workers = 1
	cSeq, _ := runStudy(t, cfgSeq)
	seq := readTrials(t, cSeq.StudyDir())

	cfgPar := pairsConfig(ds, start, end)
	cfgPar.Workers = 4
	cPar, _ := runStudy(t, cfgPar)
	par := readTrials(t, cPar.StudyDir())

	if len(seq) != len(par) {
		t.Fatalf("trial counts differ: %d vs %d", len(seq), len(par))
	}
	for i := range seq {
		for _, k := range []string{"params", "metrics", "state"} {
			a, _ := json.Marshal(seq[i][k])
			b, _ := json.Marshal(par[i][k])
			if string(a) != string(b) {
				t.Fatalf("parallel != sequential at trial %d field %q:\n seq=%s\n par=%s", i, k, a, b)
			}
		}
	}
}

func TestStudyContextCancellation(t *testing.T) {
	start := calendar.NewDate(2023, 1, 2)
	end := calendar.NewDate(2023, 12, 29)
	ds := syntheticPairs(t, start, end)
	cfg := pairsConfig(ds, start, end)
	cfg.Population = 8
	cfg.Generations = 5
	cfg.RunsDir = t.TempDir()
	cfg.StudyTS = "2026-01-02_03-04-05"

	c, err := NewCoordinator(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately
	_, err = c.Run(ctx)
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	// progress.json should be flipped to INTERRUPTED.
	pb, rerr := os.ReadFile(filepath.Join(c.StudyDir(), "progress.json"))
	if rerr != nil {
		t.Fatalf("read progress: %v", rerr)
	}
	var prog map[string]any
	_ = json.Unmarshal(pb, &prog)
	if prog["status"] != "INTERRUPTED" {
		t.Fatalf("progress status after cancel: got %v want INTERRUPTED", prog["status"])
	}
}

func TestStudyBestParamsWritten(t *testing.T) {
	start := calendar.NewDate(2023, 1, 2)
	end := calendar.NewDate(2023, 12, 29)
	ds := syntheticPairs(t, start, end)
	cfg := pairsConfig(ds, start, end)
	c, _ := runStudy(t, cfg)

	bp := filepath.Join(c.StudyDir(), "best_params", "pairs.json")
	b, err := os.ReadFile(bp)
	if err != nil {
		t.Fatalf("best_params/pairs.json not written: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("parse best_params: %v", err)
	}
	if doc["strategy"] != "pairs" {
		t.Fatalf("best_params strategy: got %v", doc["strategy"])
	}
	meta, _ := doc["metadata"].(map[string]any)
	if meta["source"] != "tuned" {
		t.Fatalf("best_params metadata.source: got %v want tuned", meta["source"])
	}
	if meta["tuned_from_study"] == nil {
		t.Fatal("best_params missing tuned_from_study")
	}
}
