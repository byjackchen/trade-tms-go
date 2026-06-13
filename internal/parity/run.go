package parity

// run.go is the parity-backtest entry point: given a script JSON and the shared
// bars.json, it runs the Go engine ZERO-COST (nautilus-compat) and dumps the
// legacy runs/<ts>/*.json artifact set (via internal/runs, byte-compatible with
// the Python reference dumper) PLUS two parity-specific artifacts the comparator
// diffs against the Nautilus dump:
//
//	fills.json   one entry per filled leg (ticker, side, qty, px, ts) — the
//	             per-fill price/qty/timing the gate compares exactly. The
//	             standard orders.json carries submitted orders, not fill prices,
//	             so we emit fills separately for an apples-to-apples diff.
//	equity.json  the per-bar total account equity curve (cash + unrealized),
//	             comparable to the Nautilus EquityCurveSamplerActor output.

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/engine"
	"github.com/byjackchen/trade-tms-go/internal/runs"
)

// RunOptions configure a parity backtest.
type RunOptions struct {
	// ScriptPath is the canonical order script JSON.
	ScriptPath string
	// BarsPath is the shared bars.json (the wrangled inputs both engines read).
	BarsPath string
	// RunsRoot is the runs/ directory the artifacts are dumped under.
	RunsRoot string
	// Timestamp names the run directory (defaults to now). The parity Makefile
	// pins it so the comparator can find a stable directory.
	Timestamp time.Time
}

// RunResult reports where artifacts landed and a few headline numbers.
type RunResult struct {
	RunDir        string
	FinalBalance  float64
	TotalPnL      float64
	NumFills      int
	BarsProcessed int
	SampledDays   int
}

// Run executes the parity backtest and dumps artifacts. It is deterministic:
// the same script + bars always produce identical output.
func Run(ctx context.Context, opts RunOptions) (*RunResult, error) {
	script, err := LoadScript(opts.ScriptPath)
	if err != nil {
		return nil, err
	}
	feed, err := LoadBarsFeed(opts.BarsPath)
	if err != nil {
		return nil, err
	}
	cfg, err := script.EngineConfig()
	if err != nil {
		return nil, err
	}
	eng, err := engine.New(ctx, cfg, feed)
	if err != nil {
		return nil, fmt.Errorf("parity: building engine: %w", err)
	}
	res, err := eng.Run(ctx)
	if err != nil {
		return nil, fmt.Errorf("parity: running engine: %w", err)
	}

	ts := opts.Timestamp
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	tsName := runs.NewRunTS(ts)
	in := runs.FromEngineResult(
		tsName, "parity",
		res.FirstTS.UTC().Format("2006-01-02"),
		res.LastTS.UTC().Format("2006-01-02"),
		res, nil, nil,
	)
	dir, err := runs.WriteArtifacts(opts.RunsRoot, in)
	if err != nil {
		return nil, fmt.Errorf("parity: writing artifacts: %w", err)
	}

	// Parity-specific extras the standard dumper does not emit.
	if err := writeFills(dir, res); err != nil {
		return nil, err
	}
	if err := writeEquityCurve(dir, res); err != nil {
		return nil, err
	}

	return &RunResult{
		RunDir:        dir,
		FinalBalance:  res.FinalBalance.Float64(),
		TotalPnL:      res.TotalPnL.Float64(),
		NumFills:      len(res.Fills),
		BarsProcessed: res.BarsProcessed,
		SampledDays:   res.SampledDays,
	}, nil
}

// writeFills emits fills.json: one entry per filled leg, in settlement order.
func writeFills(dir string, res *engine.Result) error {
	arr := runs.NewArr()
	for _, f := range res.Fills {
		arr.Append(runs.NewObj().
			Set("trade_id", runs.Str(f.TradeID)).
			Set("client_order_id", runs.Str(f.ClientOrderID)).
			Set("venue_order_id", runs.Str(f.VenueOrderID)).
			Set("strategy_id", runs.Str(f.StrategyID)).
			Set("ticker", runs.Str(f.Symbol)).
			Set("side", runs.Str(string(f.Side))).
			Set("qty", runs.Str(fmt.Sprintf("%d", int64(f.Qty)))).
			Set("px", runs.PyFloat(f.Price.Float64())).
			Set("commission_usd", runs.PyFloat(commissionUSD(f.Commission))).
			Set("ts", runs.Str(isoUTC(f.TS))).
			Set("ts_ns", runs.Int(f.TS.UTC().UnixNano())))
	}
	return atomicWriteJSON(filepath.Join(dir, "fills.json"), runs.Marshal(arr))
}

// writeEquityCurve emits equity.json: the per-bar total account equity curve.
func writeEquityCurve(dir string, res *engine.Result) error {
	arr := runs.NewArr()
	for _, p := range res.TotalEquityCurve {
		arr.Append(runs.NewObj().
			Set("ts", runs.Str(isoUTC(p.TS))).
			Set("balance_usd", runs.PyFloat(p.Value.Float64())))
	}
	return atomicWriteJSON(filepath.Join(dir, "equity.json"), runs.Marshal(arr))
}

func commissionUSD(m domain.Money) float64 { return m.Float64() }

// isoUTC formats a UTC instant as ISO 8601 with +00:00 offset (dumper form).
func isoUTC(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05-07:00") }
