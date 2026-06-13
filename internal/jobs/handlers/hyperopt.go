package handlers

// hyperopt.go is the "hyperopt.run" job handler. It runs a full NSGA-II
// walk-forward hyperparameter study through internal/hyperopt/study over a SHARED
// read-only bar dataset loaded ONCE from TimescaleDB (locked decision 5),
// persists every trial + the study progress to research.hyperopt_studies /
// research.hyperopt_trials (DB source of truth) AND emits the legacy
// runs/hyperopt/<ts>/ artifact tree (study.json / progress.json /
// trials/trial_*.json / best_params), streaming generation/trial progress to the
// jobs Redis pub channel. It is cancel-aware (ctx stops the study, writing
// INTERRUPTED) and idempotent on its study_ts (same ts + seed -> identical
// trials, upserted in place).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/data/universe"
	"github.com/byjackchen/trade-tms-go/internal/engine"
	"github.com/byjackchen/trade-tms-go/internal/hyperopt/study"
	"github.com/byjackchen/trade-tms-go/internal/jobs"
	"github.com/byjackchen/trade-tms-go/internal/params"
)

// KindHyperoptRun is the dispatch key served by Hyperopt.
const KindHyperoptRun = "hyperopt.run"

// Hyperopt handles "hyperopt.run" jobs.
//
// Payload (JSON object; unknown fields rejected):
//
//	{
//	  "strategy":     "sepa",              // sepa | sector_rotation | pairs | joint (required)
//	  "start":        "YYYY-MM-DD",        // study window start (required)
//	  "end":          "YYYY-MM-DD",        // study window end (required)
//	  "population":   50,                  // NSGA-II generation size; default 50
//	  "generations":  5,                   // generations; default 5
//	  "seed":         42,                  // PRNG seed; default 42
//	  "workers":      0,                   // eval parallelism; default min(cores-2,16)
//	  "walk_forward": true,                // default true
//	  "folds":        5,                   // default 5
//	  "embargo_days": 5,                   // default 5
//	  "tickers":      ["AAPL", ...],       // SEPA/joint stock universe (or universe window)
//	  "universe":     {"start":"...","end":"...","table":"SF1"},
//	  "starting_balance": 100000.0,        // USD; default 100000
//	  "study_ts":     "2026-..._..-..-..", // optional; default now-UTC (idempotency key)
//	  "runs_dir":     "runs/hyperopt"      // artifact base; default config.RunsDir/hyperopt
//	}
type Hyperopt struct {
	pool      *pgxpool.Pool
	uni       *universe.Store
	store     *study.Store
	loader    *params.Loader
	runsDir   string
	paramsDir string
	log       zerolog.Logger
	now       func() time.Time
}

// NewHyperopt builds the handler. runsDir is the legacy artifact base directory;
// the study tree is written under runsDir/hyperopt/<study_ts>/.
func NewHyperopt(pool *pgxpool.Pool, runsDir, paramsDir string, log zerolog.Logger) (*Hyperopt, error) {
	if pool == nil {
		return nil, errors.New("hyperopt.run: nil connection pool")
	}
	if runsDir == "" {
		runsDir = "runs"
	}
	return &Hyperopt{
		pool:      pool,
		uni:       universe.NewStore(pool),
		store:     study.NewStore(pool),
		loader:    params.NewLoader(params.DBPayloadReader{Q: pool}, paramsDir),
		runsDir:   runsDir,
		paramsDir: paramsDir,
		log:       log.With().Str("component", "hyperopt-run").Logger(),
		now:       time.Now,
	}, nil
}

// Kind implements jobs.Handler.
func (h *Hyperopt) Kind() string { return KindHyperoptRun }

type hyperoptUniverseJSON struct {
	Start string `json:"start"`
	End   string `json:"end"`
	Table string `json:"table"`
}

type hyperoptParams struct {
	Strategy        string                `json:"strategy"`
	Start           string                `json:"start"`
	End             string                `json:"end"`
	Population      int                   `json:"population"`
	Generations     int                   `json:"generations"`
	Seed            int64                 `json:"seed"`
	Workers         int                   `json:"workers"`
	WalkForward     *bool                 `json:"walk_forward"`
	Folds           int                   `json:"folds"`
	EmbargoDays     int                   `json:"embargo_days"`
	Tickers         []string              `json:"tickers"`
	Universe        *hyperoptUniverseJSON `json:"universe"`
	StartingBalance *float64              `json:"starting_balance"`
	StudyTS         string                `json:"study_ts"`
	RunsDir         string                `json:"runs_dir"`
	// TrialTimeoutSec is the per-trial timeout in whole seconds (§11). nil =>
	// default 600s; 0 => disabled; N>0 => N seconds.
	TrialTimeoutSec *int `json:"trial_timeout_sec"`
	// Resume, when true with a pinned StudyTS, resumes that study (§6.2-§6.5).
	Resume bool `json:"resume"`
}

// Run implements jobs.Handler.
func (h *Hyperopt) Run(ctx context.Context, job *jobs.Job, report jobs.ProgressFn) (any, error) {
	p, err := parseHyperoptParams(job.Payload)
	if err != nil {
		return nil, err
	}
	cfg, err := h.buildConfig(ctx, p)
	if err != nil {
		return nil, err
	}
	log := h.log.With().Int64("job_id", job.ID).Str("strategy", cfg.Strategy).Logger()

	// Load the SHARED read-only dataset ONCE (locked decision 5). The trading
	// universe is the SEPA stocks (sepa/joint) plus the strategy's derived
	// instruments (sector ETFs / pair legs) plus SPY, SPY first.
	tickers, sepaStocks, err := h.resolveUniverse(ctx, p, cfg.Strategy)
	if err != nil {
		return nil, err
	}
	feed := engine.NewStoreFeed(h.uni)
	ds, err := study.LoadDataset(ctx, feed, tickers, cfg.Start, cfg.End)
	if err != nil {
		return nil, fmt.Errorf("hyperopt.run: loading shared dataset: %w", err)
	}
	cfg.Dataset = ds
	cfg.SEPAStocks = sepaStocks
	cfg.RunsDir = h.studyRunsDir(p)
	if cfg.Resume {
		cfg.ResumeSource = h.store // §6.3 guard + §6.5 completed-trial replay
	}

	// Sink: DB store + a Redis-progress publisher wired into the jobs report fn.
	sink := &reportingSink{
		store:  h.store,
		report: report,
		ctx:    ctx,
		log:    log,
	}
	coord, err := study.NewCoordinator(cfg, sink)
	if err != nil {
		return nil, fmt.Errorf("hyperopt.run: building coordinator: %w", err)
	}

	if rerr := report(ctx, map[string]any{
		"phase": "study", "study_ts": coord.StudyTS(), "status": "RUNNING",
		"total_trials": cfg.Population * cfg.Generations, "completed_trials": 0,
	}); rerr != nil && ctx.Err() == nil {
		log.Warn().Err(rerr).Msg("initial progress report failed; continuing")
	}

	res, err := coord.Run(ctx)
	if err != nil {
		return nil, fmt.Errorf("hyperopt.run: study run: %w", err) // includes ctx.Canceled
	}

	log.Info().Str("study_ts", res.StudyTS).Int("pareto", len(res.Pareto)).Msg("hyperopt study complete")
	if rerr := report(ctx, map[string]any{
		"phase": "done", "study_ts": res.StudyTS, "status": "COMPLETE",
		"pareto_front": len(res.Pareto),
	}); rerr != nil && ctx.Err() == nil {
		log.Warn().Err(rerr).Msg("done progress report failed")
	}

	return map[string]any{
		"study_name":   res.StudyName,
		"study_ts":     res.StudyTS,
		"study_dir":    res.StudyDir,
		"strategy":     cfg.Strategy,
		"total_trials": cfg.Population * cfg.Generations,
		"pareto_front": len(res.Pareto),
	}, nil
}

// buildConfig validates the payload into a study.Config (sans dataset/universe).
func (h *Hyperopt) buildConfig(_ context.Context, p hyperoptParams) (study.Config, error) {
	switch p.Strategy {
	case "sepa", "sector_rotation", "pairs", "joint":
	default:
		return study.Config{}, fmt.Errorf("hyperopt.run: unknown strategy %q (want sepa|sector_rotation|pairs|joint)", p.Strategy)
	}
	start, err := calendar.ParseDate(p.Start)
	if err != nil {
		return study.Config{}, fmt.Errorf("hyperopt.run: invalid start %q (want YYYY-MM-DD): %w", p.Start, err)
	}
	end, err := calendar.ParseDate(p.End)
	if err != nil {
		return study.Config{}, fmt.Errorf("hyperopt.run: invalid end %q (want YYYY-MM-DD): %w", p.End, err)
	}
	if p.Resume && p.StudyTS == "" {
		return study.Config{}, errors.New("hyperopt.run: resume requires \"study_ts\" (the study to resume)")
	}
	wf := true
	if p.WalkForward != nil {
		wf = *p.WalkForward
	}
	bal := 0.0
	if p.StartingBalance != nil {
		bal = *p.StartingBalance
	}
	gens := p.Generations
	if gens == 0 {
		gens = 5
	}
	return study.Config{
		Strategy:        p.Strategy,
		Start:           start,
		End:             end,
		Population:      p.Population,
		Generations:     gens,
		Seed:            p.Seed,
		Workers:         p.Workers,
		WalkForward:     wf,
		Folds:           p.Folds,
		EmbargoDays:     p.EmbargoDays,
		StartingBalance: bal,
		StudyTS:         p.StudyTS,
		TrialTimeout:    trialTimeoutFromPayload(p.TrialTimeoutSec),
		Resume:          p.Resume,
	}, nil
}

// trialTimeoutFromPayload maps the payload's *int seconds to a study.Config
// TrialTimeout (§11): nil => 0 (Config defaults to 600s); 0 => a negative
// sentinel (disabled); N>0 => N seconds.
func trialTimeoutFromPayload(secs *int) time.Duration {
	if secs == nil {
		return 0 // Config default (600s)
	}
	if *secs <= 0 {
		return -1 // disabled
	}
	return time.Duration(*secs) * time.Second
}

// resolveUniverse returns the full registration-ordered ticker list for the
// shared dataset (SPY first, then strategy instruments, then stocks) and the SEPA
// stock universe. For sepa/joint the stocks come from the payload tickers /
// universe window; for sector/pairs the instruments come from the baseline params.
func (h *Hyperopt) resolveUniverse(ctx context.Context, p hyperoptParams, strategy string) (tickers, sepaStocks []string, err error) {
	stocks, err := h.resolveStocks(ctx, p)
	if err != nil {
		return nil, nil, err
	}
	extra := []string{"SPY"}
	switch strategy {
	case "sepa":
		if len(stocks) == 0 {
			return nil, nil, errors.New("hyperopt.run: sepa study needs a stock universe (\"tickers\" or \"universe\")")
		}
	case "sector_rotation":
		sp, _, e := h.loader.SectorRotation(ctx)
		if e != nil {
			return nil, nil, fmt.Errorf("hyperopt.run: resolve sector params: %w", e)
		}
		extra = append(extra, sp.Universe...)
	case "pairs":
		sp, _, e := h.loader.Pairs(ctx)
		if e != nil {
			return nil, nil, fmt.Errorf("hyperopt.run: resolve pairs params: %w", e)
		}
		for _, pr := range sp.Pairs {
			extra = append(extra, pr.LongLeg, pr.ShortLeg)
		}
	case "joint":
		if len(stocks) == 0 {
			return nil, nil, errors.New("hyperopt.run: joint study needs a stock universe (\"tickers\" or \"universe\")")
		}
		srp, _, e := h.loader.SectorRotation(ctx)
		if e != nil {
			return nil, nil, fmt.Errorf("hyperopt.run: resolve sector params: %w", e)
		}
		extra = append(extra, srp.Universe...)
		pp, _, e := h.loader.Pairs(ctx)
		if e != nil {
			return nil, nil, fmt.Errorf("hyperopt.run: resolve pairs params: %w", e)
		}
		for _, pr := range pp.Pairs {
			extra = append(extra, pr.LongLeg, pr.ShortLeg)
		}
	}
	tickers = dedupKeepOrder(append(extra, stocks...))
	return tickers, stocks, nil
}

// resolveStocks returns the SEPA stock universe from the explicit ticker list or
// a universe window (mutually exclusive).
func (h *Hyperopt) resolveStocks(ctx context.Context, p hyperoptParams) ([]string, error) {
	if len(p.Tickers) > 0 {
		if p.Universe != nil {
			return nil, errors.New("hyperopt.run: supply either \"tickers\" or \"universe\", not both")
		}
		return p.Tickers, nil
	}
	if p.Universe == nil {
		return nil, nil
	}
	us, err := calendar.ParseDate(p.Universe.Start)
	if err != nil {
		return nil, fmt.Errorf("hyperopt.run: invalid universe.start %q: %w", p.Universe.Start, err)
	}
	ue, err := calendar.ParseDate(p.Universe.End)
	if err != nil {
		return nil, fmt.Errorf("hyperopt.run: invalid universe.end %q: %w", p.Universe.End, err)
	}
	table := p.Universe.Table
	switch table {
	case universe.TableAny, universe.TableSF1, universe.TableSFP:
	default:
		return nil, fmt.Errorf("hyperopt.run: invalid universe.table %q (want \"\", \"SF1\" or \"SFP\")", table)
	}
	return h.uni.ListUniverseForWindow(ctx, us, ue, table)
}

// studyRunsDir returns the artifact base directory for the study tree
// (runsDir/hyperopt or the payload override).
func (h *Hyperopt) studyRunsDir(p hyperoptParams) string {
	if p.RunsDir != "" {
		return p.RunsDir
	}
	return h.runsDir + "/hyperopt"
}

// dedupKeepOrder dedupes preserving first-seen order, dropping empties.
func dedupKeepOrder(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
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

// parseHyperoptParams strictly decodes the payload (unknown fields rejected).
func parseHyperoptParams(payload json.RawMessage) (hyperoptParams, error) {
	var p hyperoptParams
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return p, fmt.Errorf("hyperopt.run: invalid payload: %w", err)
	}
	if p.Strategy == "" {
		return p, errors.New("hyperopt.run: payload requires \"strategy\"")
	}
	if p.Start == "" || p.End == "" {
		return p, errors.New("hyperopt.run: payload requires \"start\" and \"end\" (YYYY-MM-DD)")
	}
	return p, nil
}

// ---------------------------------------------------------------------------
// reportingSink: study.Sink that writes the DB store AND publishes progress to
// the jobs Redis channel via the handler's report fn.
// ---------------------------------------------------------------------------

type reportingSink struct {
	store  *study.Store
	report jobs.ProgressFn
	ctx    context.Context
	log    zerolog.Logger
}

func (s *reportingSink) UpsertStudy(ctx context.Context, cfg study.StudyConfig, p study.Progress) error {
	return s.store.UpsertStudy(ctx, cfg, p)
}

func (s *reportingSink) UpsertTrial(ctx context.Context, studyTS string, t study.TrialArtifact) error {
	return s.store.UpsertTrial(ctx, studyTS, t)
}

func (s *reportingSink) Heartbeat(ctx context.Context, studyTS string, now time.Time) error {
	return s.store.Heartbeat(ctx, studyTS, now)
}

func (s *reportingSink) Progress(ctx context.Context, studyTS string, p study.Progress) error {
	if err := s.store.Progress(ctx, studyTS, p); err != nil {
		return err
	}
	total := p.TotalTrials
	pct := 0.0
	if total > 0 {
		pct = float64(p.CompletedTrials+p.FailedTrials) / float64(total) * 100.0
	}
	msg := map[string]any{
		"phase":            "study",
		"study_ts":         studyTS,
		"status":           string(p.Status),
		"completed_trials": p.CompletedTrials,
		"failed_trials":    p.FailedTrials,
		"running_trials":   p.RunningTrials,
		"total_trials":     total,
		"percent":          pct,
	}
	if p.CurrentBest != nil {
		msg["current_best"] = map[string]any{
			"trial": p.CurrentBest.Trial, "sharpe": p.CurrentBest.Sharpe, "calmar": p.CurrentBest.Calmar,
		}
	}
	if rerr := s.report(s.ctx, msg); rerr != nil && s.ctx.Err() == nil {
		s.log.Warn().Err(rerr).Msg("hyperopt progress report failed; continuing")
	}
	return nil
}

// compile-time check.
var _ study.Sink = (*reportingSink)(nil)
