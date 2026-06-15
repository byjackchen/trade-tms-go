package study

// objective.go is the backtest-based objective: given one candidate's decoded
// overrides, it runs the REAL strategy backtest once per fold over the shared
// read-only dataset (locked decision 5), computes per-fold metrics via
// internal/metrics, and aggregates them per spec §4 (concat-and-recompute over
// the stitched return curve — NEVER averages per-fold values). The objective
// vector reported to NSGA-II is (sharpe, calmar) of the aggregated curve (§1.1
// to_objectives), matching Python EXACTLY for a given param set (locked decision
// 3: identical params through identical folds -> identical metrics).
//
// Single-window mode (no folds) runs one backtest over [start, end] and reports
// that run's own metrics, with an empty folds list (§5.3).

import (
	"context"
	"fmt"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/engine"
	"github.com/byjackchen/trade-tms-go/internal/engine/strategyassembly"
	"github.com/byjackchen/trade-tms-go/internal/hyperopt"
	"github.com/byjackchen/trade-tms-go/internal/metrics"
	"github.com/byjackchen/trade-tms-go/internal/params"
	"github.com/byjackchen/trade-tms-go/internal/portfolio"
)

// EvalResult is the outcome of evaluating one candidate over all folds: the
// aggregated metrics (the objective source) plus the per-fold metrics in fold
// order (the trial_*.json `folds` payload). FoldMetrics is empty in single-window
// mode (§4.3/§5.3).
type EvalResult struct {
	Aggregated  metrics.BacktestMetrics
	FoldMetrics []metrics.BacktestMetrics
}

// Objectives returns the (sharpe, calmar) objective vector of the aggregated
// metrics (§1.1).
func (r EvalResult) Objectives() []float64 {
	s, c := r.Aggregated.Objectives()
	return []float64{s, c}
}

// Evaluator runs the backtest objective for a study. It holds the shared
// read-only dataset, the study window/folds, the resolved baseline defaults per
// sub-strategy, and the strategy selector. It is safe for concurrent use: every
// Evaluate builds its own engine over a fresh foldFeed; no mutable state is
// shared between concurrent calls (the dataset is immutable).
type Evaluator struct {
	strategy  string
	order     []string
	ds        *Dataset
	start     calendar.Date
	end       calendar.Date
	folds     []hyperopt.EvalSegment // nil/empty => single-window mode
	defaults  map[string]map[string]any
	sepaStock []string // SEPA stock universe (the trading symbols)
	startBal  float64
	spy       string
}

// EvaluatorConfig parameterizes a new Evaluator.
type EvaluatorConfig struct {
	Strategy        string                 // sepa|sector_rotation|pairs|joint
	Dataset         *Dataset               // shared read-only bars
	Start, End      calendar.Date          // study window (inclusive)
	Folds           []hyperopt.EvalSegment // nil => single full-window backtest
	Defaults        map[string]map[string]any
	SEPAStocks      []string // SEPA stock universe (sepa/joint paths)
	StartingBalance float64
	SPYSymbol       string
}

// NewEvaluator validates the config and returns a ready Evaluator.
func NewEvaluator(cfg EvaluatorConfig) (*Evaluator, error) {
	order, err := strategyOrder(cfg.Strategy)
	if err != nil {
		return nil, err
	}
	if cfg.Dataset == nil {
		return nil, fmt.Errorf("hyperopt: evaluator needs a dataset")
	}
	spy := cfg.SPYSymbol
	if spy == "" {
		spy = "SPY"
	}
	bal := cfg.StartingBalance
	if bal <= 0 {
		bal = 100000.0
	}
	return &Evaluator{
		strategy:  cfg.Strategy,
		order:     order,
		ds:        cfg.Dataset,
		start:     cfg.Start,
		end:       cfg.End,
		folds:     cfg.Folds,
		defaults:  cfg.Defaults,
		sepaStock: append([]string(nil), cfg.SEPAStocks...),
		startBal:  bal,
		spy:       spy,
	}, nil
}

// Evaluate runs the backtest objective for one decoded candidate. The decoded
// overrides (clamped, per sub-strategy) are merged onto the baseline defaults,
// typed + validated, and fed through the engine once per fold. Per-fold metrics
// are aggregated per §4. A validation/run error is returned (the trial FAILs);
// ctx cancellation surfaces as ctx.Err().
func (e *Evaluator) Evaluate(ctx context.Context, dec Decoded) (EvalResult, error) {
	in, err := e.buildAssemblyInput(dec)
	if err != nil {
		return EvalResult{}, err
	}

	if len(e.folds) == 0 {
		// Single-window mode: one backtest over [start, end]; its own metrics.
		m, err := e.runWindow(ctx, in, e.start, e.end)
		if err != nil {
			return EvalResult{}, err
		}
		return EvalResult{Aggregated: m, FoldMetrics: nil}, nil
	}

	foldMetrics := make([]metrics.BacktestMetrics, 0, len(e.folds))
	foldCurves := make([][]float64, 0, len(e.folds))
	for i, seg := range e.folds {
		if err := ctx.Err(); err != nil {
			return EvalResult{}, err
		}
		m, curve, err := e.runFold(ctx, in, seg)
		if err != nil {
			return EvalResult{}, fmt.Errorf("hyperopt: fold %d [%s..%s]: %w",
				i, dateStr(seg.TestStart), dateStr(seg.TestEnd), err)
		}
		foldMetrics = append(foldMetrics, m)
		foldCurves = append(foldCurves, curve)
	}
	agg := metrics.AggregateFolds(foldCurves, foldMetrics)
	return EvalResult{Aggregated: agg, FoldMetrics: foldMetrics}, nil
}

// buildAssemblyInput merges the decoded overrides onto the baseline defaults,
// types + validates each sub-strategy's params, and returns the
// strategyassembly.Input (minus context, which is built per-run since it depends
// on the fold window). A validation failure here FAILs the trial.
func (e *Evaluator) buildAssemblyInput(dec Decoded) (strategyassembly.Input, error) {
	in := strategyassembly.Input{
		Strategy:        assemblyStrategy(e.strategy),
		StartingBalance: e.startBal,
		SEPAStocks:      e.sepaStock,
		SPYSymbol:       e.spy,
		// P4 objective parity (locked decision 3): Python's run_backtest ALWAYS
		// gates the optimized sub-strategy under the multi-strategy portfolio
		// (SEPA 40 / Sector 30 / Pairs 20; single-name 50%, concentration 40%,
		// daily-loss 10%), even for a single-strategy trial. Use that same gate so
		// the admitted/rejected order set — and therefore the objective vector —
		// matches Python EXACTLY. ("multi"/"joint" already uses this gate.)
		MultiStrategyGate: true,
	}
	for _, sub := range e.order {
		merged := mergeOverrides(e.defaults[sub], dec.Overrides[sub])
		switch sub {
		case "sepa":
			p, err := params.SEPAFromMap(merged)
			if err != nil {
				return in, fmt.Errorf("sepa params: %w", err)
			}
			in.Params.SEPA = p
		case "sector_rotation":
			p, err := params.SectorRotationFromMap(merged)
			if err != nil {
				return in, fmt.Errorf("sector_rotation params: %w", err)
			}
			in.Params.Sector = p
		case "pairs":
			p, err := params.PairsFromMap(merged)
			if err != nil {
				return in, fmt.Errorf("pairs params: %w", err)
			}
			in.Params.Pairs = p
		}
	}
	return in, nil
}

// runWindow runs one backtest over [start, end] and returns its metrics.
func (e *Evaluator) runWindow(ctx context.Context, in strategyassembly.Input, start, end calendar.Date) (metrics.BacktestMetrics, error) {
	m, _, err := e.run(ctx, in, start, end)
	return m, err
}

// runFold runs one fold's backtest over [seg.TestStart, seg.TestEnd] and returns
// its metrics plus the fold's equity curve (for stitching, §4.1). The dump is
// always disabled for folds (§5.3).
func (e *Evaluator) runFold(ctx context.Context, in strategyassembly.Input, seg hyperopt.EvalSegment) (metrics.BacktestMetrics, []float64, error) {
	start := calendar.NewDate(seg.TestStart.Year(), seg.TestStart.Month(), seg.TestStart.Day())
	end := calendar.NewDate(seg.TestEnd.Year(), seg.TestEnd.Month(), seg.TestEnd.Day())
	return e.run(ctx, in, start, end)
}

// run assembles + runs the engine over [start, end] and computes BacktestMetrics
// from the resulting equity curve and counters. It builds the context provider
// over the SHARED dataset (SPY closes + market caps) so the SEPA regime / cap
// gate is look-ahead-safe per window, mirroring the backtest handler.
func (e *Evaluator) run(ctx context.Context, in strategyassembly.Input, start, end calendar.Date) (metrics.BacktestMetrics, []float64, error) {
	in.Context = e.buildContext(start, end)

	asm, err := strategyassembly.Assemble(in)
	if err != nil {
		return metrics.BacktestMetrics{}, nil, fmt.Errorf("assemble: %w", err)
	}
	startBal, err := domain.MoneyFromFloat64(e.startBal)
	if err != nil {
		return metrics.BacktestMetrics{}, nil, fmt.Errorf("starting balance: %w", err)
	}
	cfg := engine.Config{
		Start:              start,
		End:                end,
		StartingBalance:    startBal,
		Profile:            engine.ProfileNautilusCompat,
		Portfolio:          asm.Portfolio,
		Context:            asm.Context,
		SPYSymbol:          asm.SPYSymbol,
		PrebuiltStrategies: asm.Strategies,
	}
	cfg.Tickers = unionTickers(asm.ExtraTickers, e.sepaStock)

	// Parity-correct feed: the engine replays ONLY [start, end]. SEPA's 400d
	// warmup tail is injected OUT OF BAND (engine.WarmupConfig) and primes only
	// the SEPA SignalGenerators — never replayed through the loop, so it produces
	// no orders and no equity samples. Pairs/Sector get NO warmup (their adapters
	// are not WarmupConsumers), mirroring Python's warmup_by_ticker (SEPA stocks
	// only). SPY regime warmup is carried separately by the ContextProvider's own
	// full SPY history (buildContext loads SPY from start-500d).
	if len(e.sepaStock) > 0 && (e.strategy == "sepa" || e.strategy == "joint") {
		cfg.Warmup = &engine.WarmupConfig{
			Bars: e.ds.WarmupSlices(e.sepaStock, start, warmupDaysDefault),
		}
	}
	feed := e.ds.WindowFeed()
	eng, err := engine.New(ctx, cfg, feed)
	if err != nil {
		return metrics.BacktestMetrics{}, nil, fmt.Errorf("engine new: %w", err)
	}
	asm.BindEquity(eng)
	res, err := eng.Run(ctx)
	if err != nil {
		return metrics.BacktestMetrics{}, nil, fmt.Errorf("engine run: %w", err)
	}

	curve := equityCurveFloats(res, e.startBal)
	// Whole-book counters via the engine's single source of truth (shared with the
	// P2/P3 backtest path). num_filled_orders is derived from res.Fills (orders
	// that produced ≥1 fill = Nautilus is_closed), NOT Order.Status which the
	// engine never mutates to FILLED — matching multi_strategy_backtest.py's
	// `sum(1 for o in all_orders if o.is_closed)`.
	ec := res.Counts("")
	counts := metrics.Counts{
		NumOrders:         ec.NumOrders,
		NumFilledOrders:   ec.NumFilledOrders,
		NumRejectedOrders: ec.NumRejectedOrders,
		NumPositions:      ec.NumPositions,
	}
	m := metrics.Compute(curve, e.startBal, res.FinalBalance.Float64(), counts)
	return m, curve, nil
}

// buildContext assembles the look-ahead-safe per-bar context provider over the
// shared dataset: SPY closes drive the regime, an as-of market-cap row seeds the
// SEPA cap gate. Returns nil when SPY is absent (cold-start defaults) or the
// strategy does not consume context. Mirrors the backtest handler's buildContext
// but reads the SHARED in-memory bars (no DB).
func (e *Evaluator) buildContext(start, end calendar.Date) *portfolio.ContextProvider {
	if e.strategy != "sepa" && e.strategy != "joint" {
		return nil
	}
	spyIB, ok := e.ds.bySym[e.spy]
	if !ok || len(spyIB.Bars) == 0 {
		return nil
	}
	lo := midnight(start.AddDays(-spyWarmupDays))
	hi := midnight(end)
	spy := make([]portfolio.SPYBar, 0, len(spyIB.Bars))
	for _, b := range spyIB.Bars {
		if b.TS.Before(lo) || b.TS.After(hi) {
			continue
		}
		spy = append(spy, portfolio.SPYBar{Date: b.TS.UTC(), Close: b.Close.Float64()})
	}
	if len(spy) == 0 {
		return nil
	}
	// Real, look-ahead-safe market caps loaded once from tms.fundamentals_sf1
	// (latest SF1 datekey <= as_of per ticker), threaded onto the shared dataset
	// by the hyperopt handler. Mirror the production backtest handler's
	// buildContext EXACTLY: one as-of SF1 row dated the run start carrying the
	// latest known cap, HasMarketCap=true only when the cap is non-zero (0 ==
	// unknown; fails the SEPA rule-8 cap gate, sorts last). When no caps were
	// loaded (e.ds.MarketCap returns 0 for all), this degenerates to the old
	// cold-start behaviour — but the handler always populates them now, so SEPA
	// names clear the $500M gate and trade (the intended objective fix).
	asOf := midnight(start)
	sf1 := make([]portfolio.SF1Row, 0, len(e.sepaStock))
	for _, t := range e.sepaStock {
		mc := e.ds.MarketCap(t)
		sf1 = append(sf1, portfolio.SF1Row{
			Ticker: t, DateKey: asOf, MarketCap: mc, HasMarketCap: mc != 0,
			Dimension: "MRT", HasDimension: true,
		})
	}
	return portfolio.NewContextProvider(spy, sf1, nil, e.sepaStock, "MRT", 0)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// assemblyStrategy maps the study strategy selector to the strategyassembly
// selector. "joint" runs the canonical 3-strategy multi set.
func assemblyStrategy(strategy string) string {
	if strategy == "joint" {
		return "multi"
	}
	return strategy
}

// mergeOverrides overlays the searched override values (float64) onto a copy of
// the baseline defaults map. Override keys always exist in defaults (they are a
// subset of the parameter set), so this only replaces the searched values.
func mergeOverrides(defaults map[string]any, overrides map[string]float64) map[string]any {
	out := make(map[string]any, len(defaults))
	for k, v := range defaults {
		out[k] = v
	}
	for k, v := range overrides {
		out[k] = v
	}
	return out
}

// equityCurveFloats projects the engine's total equity curve into the []float64
// curve the metrics functions consume. Each sample is a {ts, balance} pair; the
// curve is the balances in chronological (sample) order. When the engine produced
// no samples, the degenerate curve [starting, final] is used (§1.6).
func equityCurveFloats(res *engine.Result, starting float64) []float64 {
	pts := res.TotalEquityCurve
	if len(pts) == 0 {
		return []float64{starting, res.FinalBalance.Float64()}
	}
	out := make([]float64, len(pts))
	for i, p := range pts {
		out[i] = p.Value.Float64()
	}
	return out
}

func dateStr(t time.Time) string {
	return t.UTC().Format("2006-01-02")
}

// unionTickers returns the concatenation of two ticker slices, deduped,
// preserving first-seen order (primary extras first, then the rest). Mirrors the
// backtest handler's helper so the engine registration order is identical (SPY
// first, then ETFs / legs, then the stock universe).
func unionTickers(first, second []string) []string {
	seen := make(map[string]struct{}, len(first)+len(second))
	out := make([]string, 0, len(first)+len(second))
	for _, s := range append(append([]string(nil), first...), second...) {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
