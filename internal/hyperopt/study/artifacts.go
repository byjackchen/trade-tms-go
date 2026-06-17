package study

// artifacts.go writes the legacy runs/hyperopt/<study_ts>/ artifact tree
// (study.json, progress.json, trials/trial_%04d.json, best_params/<strat>.json)
// per spec §7, §8.2. JSON is rendered with the shared pyjson encoder
// (internal/runs: 2-space indent, insertion-order keys, shortest float surface
// form, no trailing newline). Writes are atomic (tmp + fsync + rename), the
// spec's §7.1 IMPROVE applied (durable writes).
//
// Timestamps follow the spec conventions: wall-clock instants use the
// ISO-8601 UTC form (microsecond precision when sub-second, else seconds,
// +00:00 suffix; §conventions). The study directory name is the UTC
// %Y-%m-%d_%H-%M-%S study_ts.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/metrics"
	"github.com/byjackchen/trade-tms-go/internal/runs"
)

// Layout returns the canonical sub-paths under a study directory.
func studyJSONPath(dir string) string    { return filepath.Join(dir, "study.json") }
func progressJSONPath(dir string) string { return filepath.Join(dir, "progress.json") }
func trialPath(dir string, number int) string {
	return filepath.Join(dir, "trials", fmt.Sprintf("trial_%04d.json", number))
}
func bestParamsPath(dir, strat string) string {
	return filepath.Join(dir, "best_params", strat+".json")
}

// NewStudyTS returns a fresh UTC %Y-%m-%d_%H-%M-%S study timestamp / directory
// name (§6.2; identical format to a run_ts).
func NewStudyTS(now time.Time) string { return now.UTC().Format("2006-01-02_15-04-05") }

// isoUTC renders t as an ISO-8601 UTC timestamp with +00:00 offset
// (microsecond precision when sub-second, else seconds), matching §conventions.
func isoUTC(t time.Time) string {
	u := t.UTC()
	if u.Nanosecond() == 0 {
		return u.Format("2006-01-02T15:04:05+00:00")
	}
	return u.Format("2006-01-02T15:04:05.000000+00:00")
}

// ---------------------------------------------------------------------------
// study.json
// ---------------------------------------------------------------------------

func studyObj(c StudyConfig) *runs.Obj {
	dirs := runs.NewArr()
	for _, d := range c.Directions {
		dirs.Append(runs.Str(d))
	}
	objs := runs.NewArr()
	for _, o := range c.Objectives {
		objs.Append(runs.Str(o))
	}
	wf := runs.NewObj().
		Set("enabled", runs.Bool(c.WalkForward.Enabled)).
		Set("folds", runs.Int(int64(c.WalkForward.Folds))).
		Set("embargo_days", runs.Int(int64(c.WalkForward.EmbargoDays)))
	return runs.NewObj().
		Set("version", runs.Int(int64(c.Version))).
		Set("study_name", runs.Str(c.StudyName)).
		Set("strategy", runs.Str(c.Strategy)).
		Set("start", runs.Str(c.Start)).
		Set("end", runs.Str(c.End)).
		Set("directions", dirs).
		Set("objectives", objs).
		Set("seed", runs.Int(c.Seed)).
		Set("n_trials", runs.Int(int64(c.NTrials))).
		Set("workers", runs.Int(int64(c.Workers))).
		Set("walk_forward", wf).
		Set("created_at", runs.Str(isoUTC(c.CreatedAt))).
		Set("updated_at", runs.Str(isoUTC(c.UpdatedAt)))
}

// WriteStudyJSON writes study.json into dir (§7.2).
func WriteStudyJSON(dir string, c StudyConfig) error {
	return atomicWriteJSON(studyJSONPath(dir), runs.Marshal(studyObj(c)))
}

// ---------------------------------------------------------------------------
// progress.json
// ---------------------------------------------------------------------------

func progressObj(p Progress) *runs.Obj {
	o := runs.NewObj().
		Set("status", runs.Str(string(p.Status))).
		Set("completed_trials", runs.Int(int64(p.CompletedTrials))).
		Set("failed_trials", runs.Int(int64(p.FailedTrials))).
		Set("running_trials", runs.Int(int64(p.RunningTrials))).
		Set("total_trials", runs.Int(int64(p.TotalTrials))).
		Set("workers", runs.Int(int64(p.Workers))).
		Set("started_at", isoOrNull(p.StartedAt)).
		Set("updated_at", isoOrNull(p.UpdatedAt)).
		Set("last_heartbeat_at", isoOrNull(p.LastHeartbeatAt))
	if p.CoordinatorPID != nil {
		o.Set("coordinator_pid", runs.Int(int64(*p.CoordinatorPID)))
	} else {
		o.Set("coordinator_pid", runs.Null{})
	}
	if p.CurrentBest != nil {
		o.Set("current_best", runs.NewObj().
			Set("trial", runs.Int(int64(p.CurrentBest.Trial))).
			Set("sharpe", runs.PyFloat(p.CurrentBest.Sharpe)).
			Set("calmar", runs.PyFloat(p.CurrentBest.Calmar)))
	} else {
		o.Set("current_best", runs.Null{})
	}
	if p.LastError != nil {
		o.Set("last_error", runs.Str(*p.LastError))
	} else {
		o.Set("last_error", runs.Null{})
	}
	return o
}

// WriteProgressJSON writes progress.json into dir (§7.3).
func WriteProgressJSON(dir string, p Progress) error {
	return atomicWriteJSON(progressJSONPath(dir), runs.Marshal(progressObj(p)))
}

// ---------------------------------------------------------------------------
// trials/trial_%04d.json
// ---------------------------------------------------------------------------

// metricsObj renders BacktestMetrics in the exact field order / key names of the
// reference to_dict() (§1.1), used for trial metrics and per-fold payloads.
func metricsObj(m metrics.BacktestMetrics) *runs.Obj {
	return runs.NewObj().
		Set("final_balance_usd", runs.PyFloat(m.FinalBalanceUSD)).
		Set("total_pnl_usd", runs.PyFloat(m.TotalPnLUSD)).
		Set("sharpe", runs.PyFloat(m.Sharpe)).
		Set("calmar", runs.PyFloat(m.Calmar)).
		Set("max_drawdown_pct", runs.PyFloat(m.MaxDrawdownPct)).
		Set("num_orders", runs.Int(int64(m.NumOrders))).
		Set("num_filled_orders", runs.Int(int64(m.NumFilledOrders))).
		Set("num_rejected_orders", runs.Int(int64(m.NumRejectedOrders))).
		Set("num_positions", runs.Int(int64(m.NumPositions)))
}

// foldObj renders one fold payload: {"fold": idx, ...metrics...} (§4.3): the
// fold index key first, then the fold's own metrics in to_dict() field order.
func foldObj(f FoldMetric) *runs.Obj {
	o := runs.NewObj().Set("fold", runs.Int(int64(f.Fold)))
	for _, k := range metricKeyOrder {
		o.Set(k, metricVal(f.Metrics, k))
	}
	return o
}

func trialObj(t TrialArtifact) *runs.Obj {
	o := runs.NewObj().
		Set("number", runs.Int(int64(t.Number))).
		Set("strategy", runs.Str(t.Strategy)).
		Set("params", paramsValue(t.Params))
	if t.State == TrialComplete {
		o.Set("metrics", metricsObj(t.Metrics))
	} else {
		o.Set("metrics", runs.NewObj()) // {} on FAIL (§7.4)
	}
	folds := runs.NewArr()
	for _, f := range t.Folds {
		folds.Append(foldObj(f))
	}
	o.Set("folds", folds)
	o.Set("state", runs.Str(string(t.State)))
	o.Set("started_at", runs.Str(isoUTC(t.StartedAt)))
	o.Set("finished_at", isoOrNull(t.FinishedAt))
	o.Set("duration_sec", runs.PyFloat(t.DurationS))
	if t.RunDumpTS != nil {
		o.Set("run_dump_ts", runs.Str(*t.RunDumpTS))
	} else {
		o.Set("run_dump_ts", runs.Null{})
	}
	if t.Error != nil {
		o.Set("error", runs.Str(*t.Error))
	} else {
		o.Set("error", runs.Null{})
	}
	return o
}

// WriteTrialJSON writes trials/trial_%04d.json into dir (§7.4).
func WriteTrialJSON(dir string, t TrialArtifact) error {
	return atomicWriteJSON(trialPath(dir, t.Number), runs.Marshal(trialObj(t)))
}

// ---------------------------------------------------------------------------
// best_params/<strat>.json (§8.2)
// ---------------------------------------------------------------------------

// WriteBestParamsJSON writes a tuned param document (already-marshaled bytes)
// into best_params/<strat>.json. The bytes are produced by tuneBaseline (§8.2).
func WriteBestParamsJSON(dir, strat string, body []byte) error {
	return atomicWriteJSON(bestParamsPath(dir, strat), body)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// metricKeyOrder is the to_dict() key order for fold payload assembly.
var metricKeyOrder = []string{
	"final_balance_usd", "total_pnl_usd", "sharpe", "calmar", "max_drawdown_pct",
	"num_orders", "num_filled_orders", "num_rejected_orders", "num_positions",
}

func metricVal(m metrics.BacktestMetrics, k string) runs.PyValue {
	switch k {
	case "final_balance_usd":
		return runs.PyFloat(m.FinalBalanceUSD)
	case "total_pnl_usd":
		return runs.PyFloat(m.TotalPnLUSD)
	case "sharpe":
		return runs.PyFloat(m.Sharpe)
	case "calmar":
		return runs.PyFloat(m.Calmar)
	case "max_drawdown_pct":
		return runs.PyFloat(m.MaxDrawdownPct)
	case "num_orders":
		return runs.Int(int64(m.NumOrders))
	case "num_filled_orders":
		return runs.Int(int64(m.NumFilledOrders))
	case "num_rejected_orders":
		return runs.Int(int64(m.NumRejectedOrders))
	case "num_positions":
		return runs.Int(int64(m.NumPositions))
	}
	return runs.Null{}
}

// paramsValue renders a trial's params map. Values are floats/ints/strings/lists
// or (joint) nested maps. Keys are emitted in sorted order for determinism (the
// API/UI treats params as opaque; §Q7 confirms no consumer requires a specific
// order, and sorted keys make the artifact deterministic).
func paramsValue(m map[string]any) runs.PyValue {
	o := runs.NewObj()
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		o.Set(k, anyValue(m[k]))
	}
	return o
}

// anyValue converts a decoded Go value into a PyValue. Whole-number float64s
// that came from int params are kept as floats by default (the Optuna-recorded
// shape); callers that need an int store an int64.
func anyValue(v any) runs.PyValue {
	switch x := v.(type) {
	case nil:
		return runs.Null{}
	case string:
		return runs.Str(x)
	case bool:
		return runs.Bool(x)
	case int:
		return runs.Int(int64(x))
	case int64:
		return runs.Int(x)
	case float64:
		return runs.PyFloat(x)
	case map[string]any:
		return paramsValue(x)
	case []any:
		a := runs.NewArr()
		for _, it := range x {
			a.Append(anyValue(it))
		}
		return a
	case []string:
		a := runs.NewArr()
		for _, it := range x {
			a.Append(runs.Str(it))
		}
		return a
	default:
		return runs.Str(fmt.Sprintf("%v", x))
	}
}

func isoOrNull(t *time.Time) runs.PyValue {
	if t == nil {
		return runs.Null{}
	}
	return runs.Str(isoUTC(*t))
}

// atomicWriteJSON writes body to path via tmp + fsync + rename, mkdir -p parent.
func atomicWriteJSON(path string, body []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("hyperopt: mkdir %s: %w", filepath.Dir(path), err)
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("hyperopt: create %s: %w", tmp, err)
	}
	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("hyperopt: write %s: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("hyperopt: fsync %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("hyperopt: close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("hyperopt: rename %s: %w", path, err)
	}
	return nil
}
