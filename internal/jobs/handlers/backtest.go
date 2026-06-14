package handlers

// backtest.go is the "backtest.run" job handler. It runs a deterministic
// backtest through internal/engine over TimescaleDB bars, persists the result
// to the database as the source of truth (internal/runs: research.runs /
// run_metrics / equity_curves / trades) AND emits the legacy runs/{ts}/*.json
// artifact set (P2 locked decision 4), streaming bars-processed progress to the
// jobs Redis pub channel. It is cancel-aware (the engine loop checks ctx at
// every event boundary) and idempotent on its run_ts (runs.Persist replaces a
// prior run with the same ts, so a retried job converges).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/data/universe"
	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/engine"
	"github.com/byjackchen/trade-tms-go/internal/engine/strategyassembly"
	"github.com/byjackchen/trade-tms-go/internal/jobs"
	"github.com/byjackchen/trade-tms-go/internal/params"
	"github.com/byjackchen/trade-tms-go/internal/portfolio"
	"github.com/byjackchen/trade-tms-go/internal/runs"
)

// KindBacktestRun is the dispatch key served by Backtest.
const KindBacktestRun = "backtest.run"

// progressEvery throttles per-bar progress publishes to at most one per this
// many bars (plus a terminal frame), keeping the Redis channel quiet on long
// runs while staying responsive on the UI.
const progressEvery = 200

// sepaWarmupCalendarDays is the out-of-band SEPA warmup window: 400 calendar
// days before the run start (~270 trading days, above the 200-bar TrendTemplate
// / SEPA threshold), matching multi_strategy_backtest.py's
// `warmup_start = start_dt - timedelta(days=400)`.
const sepaWarmupCalendarDays = 400

// Backtest handles "backtest.run" jobs.
//
// Payload (JSON object; unknown fields rejected):
//
//	{
//	  "tickers":          ["AAPL","KO", ...],   // explicit ticker list, OR
//	  "universe":         {"start":"...","end":"...","table":"SF1"}, // universe snapshot window
//	  "start":            "YYYY-MM-DD",         // required (bar window start)
//	  "end":              "YYYY-MM-DD",         // required (bar window end)
//	  "starting_balance": 100000.0,             // USD; default 100000
//	  "fill_profile":     "nautilus-compat",    // or "realistic"; default nautilus-compat
//	  "strategy":         "scripted",           // scripted | sepa | sector_rotation | pairs | orb | multi
//	  "intents": [ {"date":"YYYY-MM-DD","ticker":"AAPL","side":"LONG","qty":100}, ... ], // scripted only
//	  "orb_symbol":       "AAPL",                // orb strategy: the single intraday instrument
//	  "kind":             "multi-strategy",     // run kind badge; optional
//	  "seed":             0,                    // reserved for stochastic models; optional
//	  "run_ts":           "2026-..._..-..-..", // optional; default now-UTC (idempotency key)
//	  "realistic":        {"slippage_bps":..,"commission_bps":..,"commission_per_share":..}
//	}
//
// The result object carries the persisted run id, run_ts and headline numbers.
type Backtest struct {
	pool      *pgxpool.Pool
	store     *runs.Store
	uni       *universe.Store
	loader    *params.Loader
	runsDir   string
	paramsDir string
	log       zerolog.Logger
	now       func() time.Time
}

// NewBacktest builds the handler. runsDir is the legacy artifact base directory
// (config.RunsDir; default "runs").
func NewBacktest(pool *pgxpool.Pool, runsDir string, log zerolog.Logger) (*Backtest, error) {
	return NewBacktestWithParamsDir(pool, runsDir, "", log)
}

// NewBacktestWithParamsDir builds the handler with an explicit strategy-params
// override directory (config.StrategyParamsDir / TMS_STRATEGY_PARAMS_DIR). The
// params loader resolves db active_params -> param_sets -> this dir -> embedded
// baseline, exactly like the Python loader.
func NewBacktestWithParamsDir(pool *pgxpool.Pool, runsDir, paramsDir string, log zerolog.Logger) (*Backtest, error) {
	if pool == nil {
		return nil, errors.New("backtest.run: nil connection pool")
	}
	if runsDir == "" {
		runsDir = "runs"
	}
	return &Backtest{
		pool:      pool,
		store:     runs.NewStore(pool),
		uni:       universe.NewStore(pool),
		loader:    params.NewLoader(params.DBPayloadReader{Q: pool}, paramsDir),
		runsDir:   runsDir,
		paramsDir: paramsDir,
		log:       log.With().Str("component", "backtest-run").Logger(),
		now:       time.Now,
	}, nil
}

// Kind implements jobs.Handler.
func (h *Backtest) Kind() string { return KindBacktestRun }

// intentJSON is one scripted intent in the payload.
type intentJSON struct {
	Date   string `json:"date"`
	Ticker string `json:"ticker"`
	Side   string `json:"side"`
	Qty    int64  `json:"qty"`
}

// universeJSON selects tickers from a stored snapshot window.
type universeJSON struct {
	Start string `json:"start"`
	End   string `json:"end"`
	Table string `json:"table"`
}

// realisticJSON configures the realistic fill model.
type realisticJSON struct {
	SlippageBps        float64 `json:"slippage_bps"`
	CommissionBps      float64 `json:"commission_bps"`
	CommissionPerShare float64 `json:"commission_per_share"`
}

// backtestParams is the payload wire shape.
type backtestParams struct {
	Tickers         []string       `json:"tickers"`
	Universe        *universeJSON  `json:"universe"`
	Start           string         `json:"start"`
	End             string         `json:"end"`
	StartingBalance *float64       `json:"starting_balance"`
	FillProfile     string         `json:"fill_profile"`
	Strategy        string         `json:"strategy"`
	ORBSymbol       string         `json:"orb_symbol"`
	Intents         []intentJSON   `json:"intents"`
	Kind            string         `json:"kind"`
	Seed            int64          `json:"seed"`
	RunTS           string         `json:"run_ts"`
	Realistic       *realisticJSON `json:"realistic"`
}

// Run implements jobs.Handler.
func (h *Backtest) Run(ctx context.Context, job *jobs.Job, report jobs.ProgressFn) (any, error) {
	p, err := parseBacktestParams(job.Payload)
	if err != nil {
		return nil, err
	}
	cfg, asm, runTS, err := h.buildConfig(ctx, p)
	if err != nil {
		return nil, err
	}
	log := h.log.With().Int64("job_id", job.ID).Str("run_ts", runTS).Logger()

	// Throttled progress reporter wired into the engine's per-bar hook.
	lastReported := -1
	cfg.Progress = func(processed, total int) {
		if processed != total && processed%progressEvery != 0 {
			return
		}
		if processed == lastReported {
			return
		}
		lastReported = processed
		pct := 0.0
		if total > 0 {
			pct = float64(processed) / float64(total) * 100.0
		}
		if rerr := report(ctx, map[string]any{
			"phase":          "run",
			"bars_processed": processed,
			"bars_total":     total,
			"percent":        pct,
		}); rerr != nil && ctx.Err() == nil {
			log.Warn().Err(rerr).Msg("progress report failed; continuing backtest")
		}
	}

	feed := engine.NewStoreFeed(h.uni)
	eng, err := engine.New(ctx, cfg, feed)
	if err != nil {
		return nil, fmt.Errorf("backtest.run: building engine: %w", err)
	}
	// Late-bind the strategy generators' equity provider to the running account
	// (real-strategy path only; nil for scripted).
	if asm != nil {
		asm.BindEquity(eng)
	}
	if rerr := report(ctx, map[string]any{
		"phase": "run", "bars_processed": 0, "bars_total": eng.TotalBars(), "percent": 0.0,
	}); rerr != nil && ctx.Err() == nil {
		log.Warn().Err(rerr).Msg("initial progress report failed; continuing")
	}

	res, err := eng.Run(ctx)
	if err != nil {
		return nil, fmt.Errorf("backtest.run: engine run: %w", err) // includes ctx.Canceled
	}

	assembled, err := runs.Assemble(res, runs.AssembleParams{
		RunTS:           runTS,
		Kind:            p.Kind,
		StartDate:       cfg.Start,
		EndDate:         cfg.End,
		Config:          job.Payload,
		StrategySummary: collectStrategySummaries(asm),
	})
	if err != nil {
		return nil, fmt.Errorf("backtest.run: assembling result: %w", err)
	}

	runID, err := h.store.Persist(ctx, assembled.Persist)
	if err != nil {
		return nil, fmt.Errorf("backtest.run: persisting run: %w", err)
	}

	artifactDir, err := runs.WriteArtifacts(h.runsDir, assembled.Artifact)
	if err != nil {
		// Artifacts are the back-compat mirror, not the source of truth: a
		// write failure is logged but does not fail the job (the DB row is
		// committed). The operator can re-dump from the DB if needed.
		log.Warn().Err(err).Msg("legacy artifact write failed; DB row is the source of truth")
		artifactDir = ""
	}

	if rerr := report(ctx, map[string]any{
		"phase": "done", "run_id": runID, "run_ts": runTS,
		"bars_processed": res.BarsProcessed, "bars_total": eng.TotalBars(), "percent": 100.0,
	}); rerr != nil && ctx.Err() == nil {
		log.Warn().Err(rerr).Msg("done progress report failed")
	}

	log.Info().Int64("run_id", runID).Int("bars", res.BarsProcessed).
		Str("final_balance", res.FinalBalance.String()).Msg("backtest complete")

	return map[string]any{
		"run_id":              runID,
		"run_ts":              runTS,
		"kind":                assembled.Persist.Kind,
		"strategies":          res.Strategies,
		"bars_processed":      res.BarsProcessed,
		"starting_balance":    res.StartingBalance.Float64(),
		"final_balance":       res.FinalBalance.Float64(),
		"total_pnl":           res.TotalPnL.Float64(),
		"sharpe":              assembled.Persist.PortfolioMetrics.Sharpe,
		"calmar":              assembled.Persist.PortfolioMetrics.Calmar,
		"max_drawdown_pct":    assembled.Persist.PortfolioMetrics.MaxDrawdownPct,
		"num_trades":          len(assembled.Persist.Trades),
		"num_orders":          len(res.Orders),
		"num_rejected_orders": len(res.RejectedOrders),
		"artifact_dir":        artifactDir,
		"profile":             string(res.Profile),
	}, nil
}

// buildConfig validates the payload and resolves it into an engine.Config plus
// the optional strategy Assembly (non-nil for the real-strategy paths, used to
// late-bind the equity provider after engine.New) and the run_ts (idempotency
// key).
func (h *Backtest) buildConfig(ctx context.Context, p backtestParams) (engine.Config, *strategyassembly.Assembly, string, error) {
	strategy := p.Strategy
	if strategy == "" {
		strategy = "scripted"
	}
	switch strategy {
	case "scripted", "sepa", "sector_rotation", "pairs", "orb", "multi":
	default:
		return engine.Config{}, nil, "", fmt.Errorf("backtest.run: unsupported strategy %q (want scripted|sepa|sector_rotation|pairs|orb|multi)", p.Strategy)
	}

	start, err := calendar.ParseDate(p.Start)
	if err != nil {
		return engine.Config{}, nil, "", fmt.Errorf("backtest.run: invalid start %q (want YYYY-MM-DD): %w", p.Start, err)
	}
	end, err := calendar.ParseDate(p.End)
	if err != nil {
		return engine.Config{}, nil, "", fmt.Errorf("backtest.run: invalid end %q (want YYYY-MM-DD): %w", p.End, err)
	}

	tickers, err := h.resolveTickers(ctx, p)
	if err != nil {
		return engine.Config{}, nil, "", err
	}
	if strategy == "scripted" && len(tickers) == 0 {
		return engine.Config{}, nil, "", errors.New("backtest.run: no tickers resolved (supply \"tickers\" or a \"universe\" window)")
	}

	startBal := domain.MustMoney("100000.00")
	if p.StartingBalance != nil {
		startBal, err = domain.MoneyFromFloat64(*p.StartingBalance)
		if err != nil {
			return engine.Config{}, nil, "", fmt.Errorf("backtest.run: invalid starting_balance: %w", err)
		}
	}

	profile := engine.FillProfile(p.FillProfile)
	if profile == "" {
		profile = engine.ProfileNautilusCompat
	}
	if !profile.IsValid() {
		return engine.Config{}, nil, "", fmt.Errorf("backtest.run: unknown fill_profile %q (want \"nautilus-compat\" or \"realistic\")", p.FillProfile)
	}

	cfg := engine.Config{
		Start:           start,
		End:             end,
		StartingBalance: startBal,
		Profile:         profile,
	}
	if p.Realistic != nil {
		commPerShare, cerr := domain.MoneyFromFloat64(p.Realistic.CommissionPerShare)
		if cerr != nil {
			return engine.Config{}, nil, "", fmt.Errorf("backtest.run: invalid commission_per_share: %w", cerr)
		}
		cfg.Realistic = engine.RealisticParams{
			SlippageBps:        p.Realistic.SlippageBps,
			CommissionPerShare: commPerShare,
			CommissionBps:      p.Realistic.CommissionBps,
		}
	}

	var asm *strategyassembly.Assembly
	if strategy == "scripted" {
		intents, ierr := buildIntents(p.Intents)
		if ierr != nil {
			return engine.Config{}, nil, "", ierr
		}
		cfg.Tickers = tickers
		cfg.Strategies = []engine.StrategySpec{{ID: "Scripted-000", Intents: intents}}
	} else {
		cfg, asm, err = h.assembleRealStrategy(ctx, strategy, cfg, tickers, p)
		if err != nil {
			return engine.Config{}, nil, "", err
		}
	}

	runTS := p.RunTS
	if runTS == "" {
		// Auto-generated key: collision-free under worker concurrency
		// (TMS_WORKER_CONCURRENCY>1). A plain second-resolution NewRunTS would let
		// two backtests claimed in the same wall-clock second share one
		// UNIQUE(run_ts) natural key, and Store.Persist's idempotent
		// DELETE-then-INSERT would silently destroy one run. An EXPLICIT p.RunTS
		// (idempotency key for a retried logical run) is honoured verbatim so
		// retries still converge instead of duplicating.
		runTS = runs.NewRunID(h.now())
	}
	return cfg, asm, runTS, nil
}

// assembleRealStrategy resolves the selected real strategy's params, builds its
// adapters + portfolio gate + context via strategyassembly, and unions the
// strategy's required instruments (ETFs / pair legs / SPY heartbeat) into the
// engine ticker registration — SPY FIRST so its bar dispatches before same-date
// stock bars (look-ahead-safe context). Mirrors multi_strategy_backtest.py.
func (h *Backtest) assembleRealStrategy(ctx context.Context, strategy string, cfg engine.Config, tickers []string, p backtestParams) (engine.Config, *strategyassembly.Assembly, error) {
	in := strategyassembly.Input{
		Strategy:        strategy,
		StartingBalance: cfg.StartingBalance.Float64(),
		SEPAStocks:      tickers,
		ORBSymbol:       p.ORBSymbol,
		SPYSymbol:       "SPY",
	}

	// Resolve only the params the selected path needs (db active_params ->
	// param_sets -> file dir -> embedded baseline).
	var err error
	switch strategy {
	case "sepa":
		if in.Params.SEPA, _, err = h.loader.SEPA(ctx); err != nil {
			return engine.Config{}, nil, fmt.Errorf("backtest.run: resolve sepa params: %w", err)
		}
		in.Context, err = h.buildContext(ctx, cfg, tickers)
		if err != nil {
			return engine.Config{}, nil, err
		}
		if cfg.Warmup, err = h.buildWarmup(ctx, cfg, tickers); err != nil {
			return engine.Config{}, nil, err
		}
	case "sector_rotation":
		if in.Params.Sector, _, err = h.loader.SectorRotation(ctx); err != nil {
			return engine.Config{}, nil, fmt.Errorf("backtest.run: resolve sector params: %w", err)
		}
	case "pairs":
		if in.Params.Pairs, _, err = h.loader.Pairs(ctx); err != nil {
			return engine.Config{}, nil, fmt.Errorf("backtest.run: resolve pairs params: %w", err)
		}
	case "orb":
		if in.ORBSymbol == "" {
			if len(tickers) == 1 {
				in.ORBSymbol = tickers[0]
			} else {
				return engine.Config{}, nil, errors.New("backtest.run: orb strategy requires \"orb_symbol\" (or exactly one ticker)")
			}
		}
		if in.Params.ORB, _, err = h.loader.IntradayBreakout(ctx); err != nil {
			return engine.Config{}, nil, fmt.Errorf("backtest.run: resolve orb params: %w", err)
		}
	case "multi":
		if in.Params.SEPA, _, err = h.loader.SEPA(ctx); err != nil {
			return engine.Config{}, nil, fmt.Errorf("backtest.run: resolve sepa params: %w", err)
		}
		if in.Params.Sector, _, err = h.loader.SectorRotation(ctx); err != nil {
			return engine.Config{}, nil, fmt.Errorf("backtest.run: resolve sector params: %w", err)
		}
		if in.Params.Pairs, _, err = h.loader.Pairs(ctx); err != nil {
			return engine.Config{}, nil, fmt.Errorf("backtest.run: resolve pairs params: %w", err)
		}
		in.Context, err = h.buildContext(ctx, cfg, tickers)
		if err != nil {
			return engine.Config{}, nil, err
		}
		if cfg.Warmup, err = h.buildWarmup(ctx, cfg, tickers); err != nil {
			return engine.Config{}, nil, err
		}
	}

	asm, err := strategyassembly.Assemble(in)
	if err != nil {
		return engine.Config{}, nil, fmt.Errorf("backtest.run: %w", err)
	}

	// Register the primary universe (SEPA stocks) PLUS the strategy's extra
	// instruments (SPY first, then ETFs / pair legs), deduped. For pure
	// sector/pairs/orb runs `tickers` may be empty; the extras carry the full
	// universe.
	cfg.Tickers = unionTickers(asm.ExtraTickers, tickers)
	cfg.Portfolio = asm.Portfolio
	cfg.Context = asm.Context
	cfg.SPYSymbol = asm.SPYSymbol
	cfg.PrebuiltStrategies = asm.Strategies
	return cfg, asm, nil
}

// buildContext assembles the look-ahead-safe per-bar context provider from the
// store: SPY closes drive the regime, store market caps seed the SEPA
// market-cap gate (as a single as-of SF1 row dated the run start so values are
// available from day 1, look-ahead-safe). Earnings blackout is left empty
// (events feed not yet surfaced in the Go store) — SEPA treats absent blackout
// as false, the safe default. Returns nil when SPY bars are unavailable (the
// strategies then run with the cold-start regime/market-cap defaults).
func (h *Backtest) buildContext(ctx context.Context, cfg engine.Config, stocks []string) (*portfolio.ContextProvider, error) {
	// SPY history for regime: pull a generous warmup window before the run so
	// the 200-bar MA is ready on day 1 (mirrors _load_spy's 500-day warmup).
	warmupStart := calendar.NewDate(cfg.Start.Year-2, cfg.Start.Month, cfg.Start.Day)
	spyRows, err := h.uni.GetBars(ctx, "SPY", warmupStart, cfg.End)
	if err != nil {
		return nil, fmt.Errorf("backtest.run: loading SPY for context: %w", err)
	}
	if len(spyRows) == 0 {
		return nil, nil // no SPY -> cold-start defaults (regime neutral)
	}
	spy := make([]portfolio.SPYBar, 0, len(spyRows))
	for _, r := range spyRows {
		spy = append(spy, portfolio.SPYBar{Date: r.TS.UTC(), Close: r.Close})
	}

	caps, err := h.uni.MarketCaps(ctx, stocks)
	if err != nil {
		return nil, fmt.Errorf("backtest.run: loading market caps for context: %w", err)
	}
	asOf := time.Date(cfg.Start.Year, cfg.Start.Month, cfg.Start.Day, 0, 0, 0, 0, time.UTC)
	sf1 := make([]portfolio.SF1Row, 0, len(caps))
	for _, t := range stocks {
		mc := caps[t]
		sf1 = append(sf1, portfolio.SF1Row{
			Ticker: t, DateKey: asOf, MarketCap: mc, HasMarketCap: mc != 0,
			Dimension: "MRT", HasDimension: true,
		})
	}
	return portfolio.NewContextProvider(spy, sf1, nil, stocks, "MRT", 0), nil
}

// buildWarmup loads the out-of-band SEPA warmup tail (the 400 calendar days
// BEFORE the run window) for each SEPA stock and returns an engine.WarmupConfig.
// This mirrors multi_strategy_backtest.py:404-435,645-653: the warmup bars are
// pulled in the same pass as the run window but split off into warmup_by_ticker,
// then injected via SEPAUniverseRunner.warmup_ticker BEFORE engine.run() — they
// are NEVER replayed through the engine (no orders, no equity samples). Only
// SEPA stocks get warmed (Pairs/Sector are not WarmupConsumers). Returns nil
// when no stock has pre-window history (cold start), which is a no-op.
func (h *Backtest) buildWarmup(ctx context.Context, cfg engine.Config, stocks []string) (*engine.WarmupConfig, error) {
	if len(stocks) == 0 {
		return nil, nil
	}
	warmupStart := cfg.Start.AddDays(-sepaWarmupCalendarDays)
	// The bar strictly before the run window: load [warmupStart, Start] then
	// drop any bar dated >= Start (run-window bars belong to the engine feed).
	runStart := time.Date(cfg.Start.Year, cfg.Start.Month, cfg.Start.Day, 0, 0, 0, 0, time.UTC)
	bars := make(map[string][]domain.Bar, len(stocks))
	for _, t := range stocks {
		rows, err := h.uni.GetBars(ctx, t, warmupStart, cfg.Start)
		if err != nil {
			return nil, fmt.Errorf("backtest.run: loading %s warmup: %w", t, err)
		}
		hist := make([]domain.Bar, 0, len(rows))
		for _, r := range rows {
			if !r.TS.UTC().Before(runStart) {
				continue // run-window bar; the engine feed owns it
			}
			// Skip NaN rows (source NULL) exactly as the StoreFeed does — they
			// cannot be a valid OHLC bar.
			if math.IsNaN(r.Open) || math.IsNaN(r.High) || math.IsNaN(r.Low) ||
				math.IsNaN(r.Close) || math.IsNaN(r.Volume) {
				continue
			}
			bar, werr := engine.WrangleOHLCV(t, r)
			if werr != nil {
				return nil, fmt.Errorf("backtest.run: wrangling %s warmup bar: %w", t, werr)
			}
			hist = append(hist, bar)
		}
		sort.SliceStable(hist, func(i, j int) bool { return hist[i].TS.Before(hist[j].TS) })
		if len(hist) > 0 {
			bars[t] = hist
		}
	}
	if len(bars) == 0 {
		return nil, nil
	}
	return &engine.WarmupConfig{Bars: bars}, nil
}

// collectStrategySummaries gathers each real strategy's end-of-run state summary
// (engine.StateSummarizer) keyed by distinct strategy id, for the legacy
// strategy_summaries/{id}.json artifact (mirrors multi_strategy_backtest.py's
// strategy_summaries dump). For SEPA's per-symbol universe the FIRST instance's
// summary represents the id (the Python side stores a universe aggregate; a
// per-symbol sample is a faithful-enough Go stand-in). Returns nil for the
// scripted path (asm == nil) so no summaries are written.
func collectStrategySummaries(asm *strategyassembly.Assembly) map[string]map[string]any {
	if asm == nil || len(asm.Strategies) == 0 {
		return nil
	}
	out := make(map[string]map[string]any, len(asm.Strategies))
	for _, st := range asm.Strategies {
		id := st.ID()
		if _, done := out[id]; done {
			continue // first instance per id wins (SEPA universe)
		}
		summ, ok := st.(engine.StateSummarizer)
		if !ok {
			continue
		}
		m, err := toJSONMap(summ.StateSummaryJSON())
		if err != nil {
			continue // observability is best-effort; never fail the run
		}
		out[id] = m
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// toJSONMap round-trips an arbitrary JSON-serializable value into a
// map[string]any (the shape runs.AssembleParams.StrategySummary expects).
func toJSONMap(v any) (map[string]any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// unionTickers returns the concatenation of the two ticker slices, deduped,
// preserving first-seen order (primary extras first, then the rest).
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

// resolveTickers returns the explicit ticker list or, when a universe window is
// given, the survivor-bias-free universe for that window.
func (h *Backtest) resolveTickers(ctx context.Context, p backtestParams) ([]string, error) {
	if len(p.Tickers) > 0 {
		if p.Universe != nil {
			return nil, errors.New("backtest.run: supply either \"tickers\" or \"universe\", not both")
		}
		return p.Tickers, nil
	}
	if p.Universe == nil {
		return nil, nil
	}
	us, err := calendar.ParseDate(p.Universe.Start)
	if err != nil {
		return nil, fmt.Errorf("backtest.run: invalid universe.start %q: %w", p.Universe.Start, err)
	}
	ue, err := calendar.ParseDate(p.Universe.End)
	if err != nil {
		return nil, fmt.Errorf("backtest.run: invalid universe.end %q: %w", p.Universe.End, err)
	}
	table := p.Universe.Table
	switch table {
	case universe.TableAny, universe.TableSF1, universe.TableSFP:
	default:
		return nil, fmt.Errorf("backtest.run: invalid universe.table %q (want \"\", \"SF1\" or \"SFP\")", table)
	}
	return h.uni.ListUniverseForWindow(ctx, us, ue, table)
}

// buildIntents converts payload intents into engine.Intents.
func buildIntents(in []intentJSON) ([]engine.Intent, error) {
	out := make([]engine.Intent, 0, len(in))
	for i, it := range in {
		d, err := calendar.ParseDate(it.Date)
		if err != nil {
			return nil, fmt.Errorf("backtest.run: intent %d invalid date %q: %w", i, it.Date, err)
		}
		side, err := domain.ParseSignalSide(it.Side)
		if err != nil {
			return nil, fmt.Errorf("backtest.run: intent %d invalid side %q: %w", i, it.Side, err)
		}
		out = append(out, engine.Intent{
			Date:   time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, time.UTC),
			Ticker: it.Ticker,
			Side:   side,
			Qty:    domain.Qty(it.Qty),
		})
	}
	return out, nil
}

// parseBacktestParams strictly decodes the payload (unknown fields rejected).
func parseBacktestParams(payload json.RawMessage) (backtestParams, error) {
	var p backtestParams
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return p, fmt.Errorf("backtest.run: invalid payload: %w", err)
	}
	if p.Start == "" || p.End == "" {
		return p, errors.New("backtest.run: payload requires \"start\" and \"end\" (YYYY-MM-DD)")
	}
	return p, nil
}
