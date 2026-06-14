package runs

// artifacts.go writes the legacy runs/{ts}/*.json artifact set, byte-compatible
// with the Python reference dumper (src/runs/dumper.py) as specified in
// api-ws-redis.md §7 [MUST-MATCH] (locked decision 4: emit alongside the DB
// source of truth for parity diffing + UI back-compat).
//
// Layout (api-ws-redis.md §7):
//
//	runs/{ts}/
//	  meta.json
//	  orders.json
//	  positions.json
//	  account.json
//	  regime_samples.json
//	  strategy_summaries/{sanitized_strategy_id}.json
//	  strategy_equity/{sanitized_strategy_id}.json   # only when non-empty
//
// {ts} is the UTC %Y-%m-%d_%H-%M-%S directory name. All files use
// json.dumps(payload, indent=2)-compatible bytes (no trailing newline), with
// Python repr float surface form (pyjson.go).

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/engine"
)

// SchemaVersion is the meta.json schema version (dumper.py SCHEMA_VERSION = 1).
const SchemaVersion = 1

// ArtifactInput is everything the artifact writer needs. It is derived from an
// engine.Result by FromEngineResult; callers may add regime samples /
// strategy summaries the engine does not produce.
type ArtifactInput struct {
	TS              string // UTC %Y-%m-%d_%H-%M-%S directory name
	Kind            string // run kind badge (default "multi-strategy")
	StartDate       string // YYYY-MM-DD
	EndDate         string // YYYY-MM-DD
	StartingBalance domain.Money
	FinalBalance    domain.Money
	TotalPnL        domain.Money
	Strategies      []string

	Orders          []domain.Order
	Positions       []domain.Position
	AccountHistory  []engine.AccountStatePoint
	RegimeSamples   map[string]int
	StrategyEquity  map[string][]EquityPoint
	StrategySummary map[string]map[string]any // strategy_id -> opaque end-of-run summary
}

// EquityPoint is one {ts, balance_usd} artifact sample (account.json /
// strategy_equity shape, §7.4/§7.7).
type EquityPoint struct {
	TS         time.Time
	BalanceUSD domain.Money
}

// FromEngineResult projects an engine.Result into an ArtifactInput. The
// per-strategy equity curves carry cumulative realized+unrealized PnL in USD
// (§7.7). regimeSamples / strategySummary are optional (the scripted engine
// produces neither; pass nil for empty {}).
func FromEngineResult(ts, kind, start, end string, res *engine.Result,
	regimeSamples map[string]int, strategySummary map[string]map[string]any) ArtifactInput {

	se := make(map[string][]EquityPoint, len(res.StrategyEquity))
	for sid, pts := range res.StrategyEquity {
		out := make([]EquityPoint, len(pts))
		for i, p := range pts {
			out[i] = EquityPoint{TS: p.TS, BalanceUSD: p.Value}
		}
		se[sid] = out
	}
	if kind == "" {
		kind = "multi-strategy"
	}
	return ArtifactInput{
		TS:              ts,
		Kind:            kind,
		StartDate:       start,
		EndDate:         end,
		StartingBalance: res.StartingBalance,
		FinalBalance:    res.FinalBalance,
		TotalPnL:        res.TotalPnL,
		Strategies:      res.Strategies,
		Orders:          res.Orders,
		Positions:       res.Positions,
		AccountHistory:  res.AccountStates,
		RegimeSamples:   regimeSamples,
		StrategyEquity:  se,
		StrategySummary: strategySummary,
	}
}

// WriteArtifacts writes the full artifact set under baseDir/{ts}/ and returns
// the created directory. baseDir defaults to "runs" (TMS_RUNS_DIR). The write
// is best-effort atomic per file (write tmp + rename) so a partially written
// run never corrupts an existing one.
func WriteArtifacts(baseDir string, in ArtifactInput) (string, error) {
	if in.TS == "" {
		return "", fmt.Errorf("runs: artifact input has empty ts")
	}
	if baseDir == "" {
		baseDir = "runs"
	}
	outDir := filepath.Join(baseDir, in.TS)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", fmt.Errorf("runs: mkdir %s: %w", outDir, err)
	}

	files := map[string][]byte{
		"meta.json":           Marshal(metaObj(in)),
		"orders.json":         Marshal(ordersArr(in.Orders)),
		"positions.json":      Marshal(positionsArr(in.Positions)),
		"account.json":        Marshal(accountArr(in.AccountHistory)),
		"regime_samples.json": Marshal(regimeObj(in.RegimeSamples)),
	}
	for name, body := range files {
		if err := atomicWrite(filepath.Join(outDir, name), body); err != nil {
			return "", err
		}
	}

	// strategy_summaries/ — always created (mirrors the reference, which makes
	// the dir even when empty).
	sumDir := filepath.Join(outDir, "strategy_summaries")
	if err := os.MkdirAll(sumDir, 0o755); err != nil {
		return "", fmt.Errorf("runs: mkdir %s: %w", sumDir, err)
	}
	for _, sid := range SortedKeys(in.StrategySummary) {
		body := Marshal(strategySummaryArr(in.TS, in.StrategySummary[sid]))
		if err := atomicWrite(filepath.Join(sumDir, sanitizeID(sid)+".json"), body); err != nil {
			return "", err
		}
	}

	// strategy_equity/ — only when at least one strategy has points (§7.7).
	if hasEquity(in.StrategyEquity) {
		eqDir := filepath.Join(outDir, "strategy_equity")
		if err := os.MkdirAll(eqDir, 0o755); err != nil {
			return "", fmt.Errorf("runs: mkdir %s: %w", eqDir, err)
		}
		for _, sid := range SortedKeys(in.StrategyEquity) {
			pts := in.StrategyEquity[sid]
			if len(pts) == 0 {
				continue
			}
			body := Marshal(equityArr(pts))
			if err := atomicWrite(filepath.Join(eqDir, sanitizeID(sid)+".json"), body); err != nil {
				return "", err
			}
		}
	}
	return outDir, nil
}

// metaObj builds meta.json (§7.1).
func metaObj(in ArtifactInput) *Obj {
	strats := NewArr()
	for _, s := range in.Strategies {
		strats.Append(Str(s))
	}
	return NewObj().
		Set("version", Int(SchemaVersion)).
		Set("ts", Str(in.TS)).
		Set("start_date", Str(in.StartDate)).
		Set("end_date", Str(in.EndDate)).
		Set("starting_balance_usd", PyFloat(in.StartingBalance.Float64())).
		Set("final_balance_usd", PyFloat(in.FinalBalance.Float64())).
		Set("total_pnl_usd", PyFloat(in.TotalPnL.Float64())).
		Set("strategies", strats).
		Set("kind", Str(in.Kind))
}

// accountArr builds account.json: [{"ts": ISO+00:00, "balance_usd": float}]
// (§7.4). One point per recorded AccountState.
func accountArr(hist []engine.AccountStatePoint) *Arr {
	a := NewArr()
	for _, p := range hist {
		a.Append(NewObj().
			Set("ts", Str(isoUTC(p.TS))).
			Set("balance_usd", PyFloat(p.BalanceUSD.Float64())))
	}
	return a
}

// equityArr builds a strategy_equity/{id}.json array (§7.7).
func equityArr(pts []EquityPoint) *Arr {
	a := NewArr()
	for _, p := range pts {
		a.Append(NewObj().
			Set("ts", Str(isoUTC(p.TS))).
			Set("balance_usd", PyFloat(p.BalanceUSD.Float64())))
	}
	return a
}

// regimeObj builds regime_samples.json: {regime_label: count} (§7.5). Keys are
// emitted in sorted order for determinism (the API reads it as a plain object).
func regimeObj(samples map[string]int) *Obj {
	o := NewObj()
	for _, k := range SortedKeys(samples) {
		o.Set(k, Int(int64(samples[k])))
	}
	return o
}

// strategySummaryArr builds a strategy_summaries/{id}.json array: exactly one
// end-of-run sample {ts, summary} (§7.6).
func strategySummaryArr(ts string, summary map[string]any) *Arr {
	a := NewArr()
	a.Append(NewObj().
		Set("ts", Str(tsToISO(ts))).
		Set("summary", fromAny(summary)))
	return a
}

// ordersArr builds orders.json from the engine's domain.Orders. The reference
// dumps full Nautilus order serializations; the Go engine emits the subset it
// genuinely models (the API passes these through opaquely, §7.2). Quantities
// are strings, prices numbers, matching the reference field typing.
func ordersArr(orders []domain.Order) *Arr {
	a := NewArr()
	for _, o := range orders {
		obj := NewObj().
			Set("client_order_id", Str(o.ClientOrderID)).
			Set("strategy_id", Str(o.StrategyID)).
			Set("instrument_id", Str(o.Symbol)).
			Set("type", Str(string(o.Type))).
			Set("side", Str(string(o.Side))).
			Set("quantity", Str(fmt.Sprintf("%d", o.Qty))).
			Set("time_in_force", Str(string(o.TIF))).
			Set("status", Str(string(o.Status))).
			Set("ts_init", Int(o.TS.UnixNano()))
		if o.Reason != "" {
			obj.Set("reason", Str(o.Reason))
		}
		a.Append(obj)
	}
	return a
}

// positionsArr builds positions.json from the engine's final position
// snapshots (§7.3 pass-through). signed_qty is a number, quantity a string.
func positionsArr(positions []domain.Position) *Arr {
	a := NewArr()
	for _, p := range positions {
		side := "FLAT"
		switch {
		case p.IsLong():
			side = "LONG"
		case p.IsShort():
			side = "SHORT"
		}
		qty := int64(p.SignedQty)
		if qty < 0 {
			qty = -qty
		}
		a.Append(NewObj().
			Set("position_id", Str(p.Symbol+"-"+p.StrategyID)).
			Set("strategy_id", Str(p.StrategyID)).
			Set("instrument_id", Str(p.Symbol)).
			Set("side", Str(side)).
			Set("signed_qty", PyFloat(float64(p.SignedQty))).
			Set("quantity", Str(fmt.Sprintf("%d", qty))).
			Set("avg_px_open", PyFloat(p.AvgPx.Float64())).
			Set("realized_pnl", Str(p.RealizedPnL.StringFixed(2)+" USD")).
			Set("ts_last", Int(p.UpdatedAt.UnixNano())))
	}
	return a
}

// fromAny converts an arbitrary Go value (decoded JSON / summary map) into a
// PyValue tree, preserving map key sort order for determinism.
func fromAny(v any) PyValue {
	switch x := v.(type) {
	case nil:
		return Null{}
	case PyValue:
		return x
	case string:
		return Str(x)
	case bool:
		return Bool(x)
	case int:
		return Int(int64(x))
	case int64:
		return Int(x)
	case float64:
		return PyFloat(x)
	case map[string]any:
		o := NewObj()
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			o.Set(k, fromAny(x[k]))
		}
		return o
	case []any:
		a := NewArr()
		for _, it := range x {
			a.Append(fromAny(it))
		}
		return a
	default:
		return Str(fmt.Sprintf("%v", x)) // json.dumps default=str fallback
	}
}

// sanitizeID mirrors the dumper's filename sanitization (§7): ":" -> "_",
// "/" -> "_".
func sanitizeID(id string) string {
	return strings.NewReplacer(":", "_", "/", "_").Replace(id)
}

func hasEquity(m map[string][]EquityPoint) bool {
	for _, v := range m {
		if len(v) > 0 {
			return true
		}
	}
	return false
}

// isoUTC renders t as Python datetime.isoformat() with +00:00 offset
// (microsecond precision when sub-second, else seconds), matching §7.4.
func isoUTC(t time.Time) string {
	u := t.UTC()
	if u.Nanosecond() == 0 {
		return u.Format("2006-01-02T15:04:05+00:00")
	}
	// Python isoformat uses microseconds.
	return u.Format("2006-01-02T15:04:05.000000+00:00")
}

// tsToISO converts a run-ts directory name to an ISO 8601 UTC instant (used for
// the strategy summary sample ts). It accepts both the second-resolution
// %Y-%m-%d_%H-%M-%S form (NewRunTS / explicit idempotency keys) and the
// collision-free %Y-%m-%d_%H-%M-%S-MMMMMM-CCCC form (NewRunID): the trailing
// "-MMMMMM-CCCC" microsecond+counter suffix is parsed back into sub-second
// precision so the emitted ISO instant matches the wall-clock the key encodes.
func tsToISO(ts string) string {
	base, micros, ok := splitRunID(ts)
	if t, err := time.Parse("2006-01-02_15-04-05", base); err == nil {
		if ok {
			t = t.Add(time.Duration(micros) * time.Microsecond)
		}
		return isoUTC(t.UTC())
	}
	return ts
}

// splitRunID separates a collision-free run key
// "%Y-%m-%d_%H-%M-%S-MMMMMM-CCCC" into its second-resolution base and the
// microsecond field. For a plain second-resolution key it returns the key
// unchanged with ok=false. The 4-digit counter is a uniqueness tiebreaker only
// and carries no time information, so it is discarded.
func splitRunID(ts string) (base string, micros int, ok bool) {
	// The base form has exactly two '-' inside the time portion after the '_';
	// the collision-free form appends "-MMMMMM-CCCC" → two extra '-' groups.
	parts := strings.Split(ts, "-")
	if len(parts) != 7 {
		return ts, 0, false
	}
	// parts: [YYYY, MM, DD_HH, MM, SS, MMMMMM, CCCC]
	m, err := parseDigits(parts[5], 6)
	if err != nil {
		return ts, 0, false
	}
	if _, err := parseDigits(parts[6], 4); err != nil {
		return ts, 0, false
	}
	base = strings.Join(parts[:5], "-")
	return base, m, true
}

// parseDigits parses an exactly-n-digit decimal field (leading zeros kept).
func parseDigits(s string, n int) (int, error) {
	if len(s) != n {
		return 0, fmt.Errorf("runs: field %q is not %d digits", s, n)
	}
	v := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("runs: field %q has non-digit", s)
		}
		v = v*10 + int(c-'0')
	}
	return v, nil
}

// atomicWrite writes body to path via a tmp file + rename, fsyncing the file
// before rename (the spec's §7.1 IMPROVE: durable artifact writes).
func atomicWrite(path string, body []byte) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("runs: create %s: %w", tmp, err)
	}
	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("runs: write %s: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("runs: fsync %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("runs: close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("runs: rename %s: %w", path, err)
	}
	return nil
}

// NewRunTS returns a fresh UTC run-ts directory name at SECOND resolution
// (%Y-%m-%d_%H-%M-%S). This is the exact byte-compatible Python dumper dir name
// and is used by the single-threaded parity runner (which pins an explicit
// Timestamp, so it never collides). It is NOT collision-free under concurrency
// — concurrent backtest jobs MUST use NewRunID instead.
func NewRunTS(now time.Time) string {
	return now.UTC().Format("2006-01-02_15-04-05")
}

// runIDSeq breaks exact-microsecond ties between run keys generated on different
// goroutines within the same process (two NewRunID calls that land in the same
// microsecond still differ). It is monotonic per process; combined with the
// microsecond wall-clock field it yields a strictly unique, lexically sortable
// suffix.
var runIDSeq atomic.Uint64

// NewRunID returns a COLLISION-FREE run key for concurrent backtests:
// the second-resolution NewRunTS form plus a 6-digit microsecond field and a
// 4-digit per-process monotonic counter, "-MMMMMM-CCCC".
//
// Worker concurrency is >1 (TMS_WORKER_CONCURRENCY=4), so two backtests can be
// claimed within the same wall-clock second. With a plain second-resolution
// run_ts both rows shared one UNIQUE(run_ts) natural key and the idempotent
// DELETE-then-INSERT in Store.Persist silently destroyed one run's persisted
// results + artifact dir. The microsecond field separates near-simultaneous
// runs; the monotonic counter guarantees uniqueness even if two goroutines read
// the same microsecond. The form stays lexically sortable (run list ORDER BY
// run_ts DESC keeps newest-first ordering) and is a valid directory name.
//
// Callers that supply an EXPLICIT run_ts (an idempotency key for a retried
// logical run) keep the second-resolution form and the DELETE-then-INSERT
// convergence semantics — only auto-generated keys gain the unique suffix.
func NewRunID(now time.Time) string {
	u := now.UTC()
	micros := u.Nanosecond() / 1000
	seq := runIDSeq.Add(1) % 10000
	return fmt.Sprintf("%s-%06d-%04d", u.Format("2006-01-02_15-04-05"), micros, seq)
}
