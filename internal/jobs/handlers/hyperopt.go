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

	"github.com/byjackchen/trade-tms-go/internal/composition"
	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/data/universe"
	"github.com/byjackchen/trade-tms-go/internal/engine"
	"github.com/byjackchen/trade-tms-go/internal/engine/strategyassembly"
	"github.com/byjackchen/trade-tms-go/internal/hyperopt/study"
	"github.com/byjackchen/trade-tms-go/internal/jobs"
	"github.com/byjackchen/trade-tms-go/internal/params"
	"github.com/byjackchen/trade-tms-go/internal/params/paramsdb"
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
		loader:    params.NewLoader(paramsdb.NewReader(pool), paramsDir),
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

// hyperoptCompositionRanges is the optional per-launch BE-Space range overrides
// for a composition study (decision 2). Each field is a [low, high] pair; an
// omitted field defaults to the global default range. Mirrors
// study.CompositionRanges' five dimensions.
type hyperoptCompositionRanges struct {
	Weight        *[2]float64 `json:"weight"`
	Cash          *[2]float64 `json:"cash"`
	SingleName    *[2]float64 `json:"single_name"`
	Concentration *[2]float64 `json:"concentration"`
	DailyLoss     *[2]float64 `json:"daily_loss"`
}

type hyperoptParams struct {
	// Kind selects the study flavour: "" / "strategy" = legacy single/joint
	// strategy-param tune; "composition" = tune a Composition's weights/cash/risk
	// (BE-Space). For a composition study CompositionID is required and Strategy is
	// ignored (the strategies come from the target's active members).
	Kind string `json:"kind"`
	// Ranges are the optional BE-Space range overrides for a composition study.
	Ranges   *hyperoptCompositionRanges `json:"ranges"`
	Strategy string                     `json:"strategy"`
	// CompositionID/Composition back the DORMANT joint (multi-strategy) study path:
	// no API surface enqueues them anymore (Composition-level optimize is dropped
	// from the product), but the worker code stays intact for older queued joint
	// payloads. For a joint study Composition carries the resolved blueprint whose
	// ACTIVE members + weights + risk drive assembly and the universe
	// (docs/concept-alignment.md §3.3). They are absent for a single-strategy tune
	// and for older queued joint payloads (which fall back to the default-multi seed
	// Composition).
	CompositionID   string                   `json:"composition_id"`
	Composition     *composition.Composition `json:"composition"`
	Start           string                   `json:"start"`
	End             string                   `json:"end"`
	Population      int                      `json:"population"`
	Generations     int                      `json:"generations"`
	Seed            int64                    `json:"seed"`
	Workers         int                      `json:"workers"`
	WalkForward     *bool                    `json:"walk_forward"`
	Folds           int                      `json:"folds"`
	EmbargoDays     int                      `json:"embargo_days"`
	Tickers         []string                 `json:"tickers"`
	Universe        *hyperoptUniverseJSON    `json:"universe"`
	StartingBalance *float64                 `json:"starting_balance"`
	StudyTS         string                   `json:"study_ts"`
	RunsDir         string                   `json:"runs_dir"`
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
	// Load REAL, look-ahead-safe market caps ONCE (locked decision 5: no DB during
	// the optimization loop) and attach them to the shared dataset. Without this
	// the SEPA context stubs every cap to 0, every name fails the rule-8 $500M
	// gate, and the SEPA hyperopt objective degenerates to 0 (never trades). Caps
	// come from tms.fundamentals_sf1 (universe.Store.MarketCaps: latest datekey,
	// dimension DESC tie-break), mirroring the production backtest handler.
	if len(sepaStocks) > 0 && runWantsSEPA(cfg) {
		caps, cerr := h.uni.MarketCaps(ctx, sepaStocks)
		if cerr != nil {
			return nil, fmt.Errorf("hyperopt.run: loading market caps: %w", cerr)
		}
		ds.SetMarketCaps(caps)
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
func (h *Hyperopt) buildConfig(ctx context.Context, p hyperoptParams) (study.Config, error) {
	if studyKindOf(p) == study.KindComposition {
		return h.buildCompositionConfig(ctx, p)
	}
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
	// A joint study targets a concrete Composition: the resolved blueprint the API
	// enqueues drives assembly + universe (its ACTIVE members + weights + risk),
	// replacing the old static default-multi seed (docs/concept-alignment.md §3.3).
	// Validate it up front so a malformed Composition FAILs the job cleanly instead
	// of surfacing deep inside assembly. CompositionID without Composition (older
	// queued payloads) is accepted and falls back to the seed Composition downstream.
	comp, err := resolveStudyComposition(p)
	if err != nil {
		return study.Config{}, err
	}
	return study.Config{
		Strategy:        p.Strategy,
		Composition:     comp,
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

// resolveStudyComposition returns the resolved Composition a joint study targets,
// or nil when none was enqueued (single-strategy tunes carry no Composition; the
// joint objective then falls back to the default-multi seed). A present
// Composition is validated.
func resolveStudyComposition(p hyperoptParams) (*composition.Composition, error) {
	if p.Composition == nil {
		return nil, nil
	}
	if err := p.Composition.Validate(); err != nil {
		return nil, fmt.Errorf("hyperopt.run: invalid composition: %w", err)
	}
	return p.Composition, nil
}

// studyKindOf classifies a payload into a study.StudyKind. An explicit
// "composition" kind (or a payload carrying a composition_id with no strategy)
// selects the composition path; everything else is the legacy strategy path.
func studyKindOf(p hyperoptParams) study.StudyKind {
	if p.Kind == string(study.KindComposition) {
		return study.KindComposition
	}
	return study.KindStrategy
}

// buildCompositionConfig validates a composition-study payload and resolves the
// per-member FIXED signal params (decision 4) into a study.Config. The target
// Composition (its ACTIVE members + per-member param reference) must be present in
// the payload (the API resolves it from composition_id and enqueues it). The
// BE-Space ranges default globally and are overridable from the payload (decision 2).
func (h *Hyperopt) buildCompositionConfig(ctx context.Context, p hyperoptParams) (study.Config, error) {
	if p.Composition == nil {
		return study.Config{}, errors.New("hyperopt.run: composition study requires the resolved \"composition\" blueprint")
	}
	if err := p.Composition.Validate(); err != nil {
		return study.Config{}, fmt.Errorf("hyperopt.run: invalid composition: %w", err)
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
	// Resolve each ACTIVE member's FIXED signal params (decision 4: member active
	// params; the SIGNAL params are not tuned here). NULL param_set_id => the
	// strategy's active params (what the Loader resolves).
	fixed, err := h.resolveMemberParams(ctx, *p.Composition)
	if err != nil {
		return study.Config{}, err
	}
	ranges := compositionRangesFromPayload(p.Ranges)
	return study.Config{
		Kind:              study.KindComposition,
		Strategy:          string(study.KindComposition),
		Composition:       p.Composition,
		CompositionRanges: ranges,
		FixedParams:       fixed,
		Start:             start,
		End:               end,
		Population:        p.Population,
		Generations:       gens,
		Seed:              p.Seed,
		Workers:           p.Workers,
		WalkForward:       wf,
		Folds:             p.Folds,
		EmbargoDays:       p.EmbargoDays,
		StartingBalance:   bal,
		StudyTS:           p.StudyTS,
		TrialTimeout:      trialTimeoutFromPayload(p.TrialTimeoutSec),
		Resume:            p.Resume,
	}, nil
}

// resolveMemberParams resolves the typed signal params for each ACTIVE member of
// the composition into a strategyassembly.Params (decision 4). Inactive members
// are skipped (they are not assembled). The Loader resolves each strategy's active
// params (db active_params -> file -> baseline).
func (h *Hyperopt) resolveMemberParams(ctx context.Context, comp composition.Composition) (strategyassembly.Params, error) {
	var out strategyassembly.Params
	for _, m := range comp.Members {
		if !m.Active {
			continue
		}
		switch m.StrategyID {
		case composition.StrategySEPA:
			sp, _, err := h.loader.SEPA(ctx)
			if err != nil {
				return out, fmt.Errorf("hyperopt.run: resolve sepa params: %w", err)
			}
			out.SEPA = sp
		case composition.StrategySectorRotation:
			sp, _, err := h.loader.SectorRotation(ctx)
			if err != nil {
				return out, fmt.Errorf("hyperopt.run: resolve sector params: %w", err)
			}
			out.Sector = sp
		case composition.StrategyPairs:
			sp, _, err := h.loader.Pairs(ctx)
			if err != nil {
				return out, fmt.Errorf("hyperopt.run: resolve pairs params: %w", err)
			}
			out.Pairs = sp
		case composition.StrategyIntradayBreakout:
			sp, _, err := h.loader.IntradayBreakout(ctx)
			if err != nil {
				return out, fmt.Errorf("hyperopt.run: resolve orb params: %w", err)
			}
			out.ORB = sp
		}
	}
	return out, nil
}

// compositionRangesFromPayload overlays any payload range overrides onto the
// global defaults (decision 2). A nil payload (or nil field) keeps the default.
func compositionRangesFromPayload(r *hyperoptCompositionRanges) *study.CompositionRanges {
	out := study.DefaultCompositionRanges()
	if r == nil {
		return &out
	}
	if r.Weight != nil {
		out.WeightLow, out.WeightHigh = r.Weight[0], r.Weight[1]
	}
	if r.Cash != nil {
		out.CashLow, out.CashHigh = r.Cash[0], r.Cash[1]
	}
	if r.SingleName != nil {
		out.SingleLow, out.SingleHigh = r.SingleName[0], r.SingleName[1]
	}
	if r.Concentration != nil {
		out.ConcLow, out.ConcHigh = r.Concentration[0], r.Concentration[1]
	}
	if r.DailyLoss != nil {
		out.DailyLow, out.DailyHigh = r.DailyLoss[0], r.DailyLoss[1]
	}
	return &out
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
	case "joint", string(study.KindComposition):
		// The universe reflects the Composition's ACTIVE members: a SEPA member needs
		// a stock universe, a sector member adds its ETFs, a pairs member adds its
		// legs (docs/concept-alignment.md §3.3). A composition study ALWAYS carries a
		// resolved Composition; older queued joint payloads carry none and fall back to
		// the canonical 3-strategy blend (all of SEPA + sector + pairs), matching the
		// prior default-multi behaviour.
		wantSEPA := jointMemberActive(p.Composition, composition.StrategySEPA)
		wantSector := jointMemberActive(p.Composition, composition.StrategySectorRotation)
		wantPairs := jointMemberActive(p.Composition, composition.StrategyPairs)
		if wantSEPA && len(stocks) == 0 {
			return nil, nil, errors.New("hyperopt.run: joint study needs a stock universe (\"tickers\" or \"universe\")")
		}
		if wantSector {
			srp, _, e := h.loader.SectorRotation(ctx)
			if e != nil {
				return nil, nil, fmt.Errorf("hyperopt.run: resolve sector params: %w", e)
			}
			extra = append(extra, srp.Universe...)
		}
		if wantPairs {
			pp, _, e := h.loader.Pairs(ctx)
			if e != nil {
				return nil, nil, fmt.Errorf("hyperopt.run: resolve pairs params: %w", e)
			}
			for _, pr := range pp.Pairs {
				extra = append(extra, pr.LongLeg, pr.ShortLeg)
			}
		}
		if !wantSEPA {
			// No SEPA member: the stock universe is not a trading universe; drop it
			// so it is not registered (and not loaded for market caps below).
			stocks = nil
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

// runWantsSEPA reports whether a study's strategy set includes SEPA (the driver
// for the look-ahead-safe market-cap load): the sepa/joint selector on the
// strategy path, or an ACTIVE sepa member of the target Composition on the
// composition path.
func runWantsSEPA(cfg study.Config) bool {
	if cfg.Kind == study.KindComposition {
		return jointMemberActive(cfg.Composition, composition.StrategySEPA)
	}
	return cfg.Strategy == "sepa" || cfg.Strategy == "joint"
}

// jointMemberActive reports whether a joint study's Composition has the given
// strategy as an ACTIVE member. A nil Composition (older queued payloads with no
// enqueued blueprint) is treated as the canonical 3-strategy default-multi blend,
// so every strategy is active — preserving the prior behaviour.
func jointMemberActive(m *composition.Composition, strategyID string) bool {
	if m == nil {
		return true
	}
	for _, mem := range m.Members {
		if mem.StrategyID == strategyID {
			return mem.Active
		}
	}
	return false
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
	// A composition study carries no strategy (the strategies come from the target's
	// active members); it requires composition_id / composition instead.
	if studyKindOf(p) == study.KindComposition {
		if p.CompositionID == "" && p.Composition == nil {
			return p, errors.New("hyperopt.run: composition study requires \"composition_id\"")
		}
	} else if p.Strategy == "" {
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
