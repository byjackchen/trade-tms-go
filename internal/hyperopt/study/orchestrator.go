package study

// orchestrator.go is the study coordinator (spec §6): given a strategy, search
// space, window, walk-forward config, population/generations, seed and
// parallelism, it runs the self-written NSGA-II (internal/hyperopt/nsga2) where
// each candidate's objective vector is the aggregate of per-fold backtest metrics
// over a SHARED read-only bar dataset (locked decision 5). It streams trial
// artifacts + DB rows + progress as trials complete, writes best_params on
// COMPLETE, and is fully ctx-cancellation aware with bounded memory and no leaked
// goroutines.
//
// Determinism (locked decision 1/3/5): ONE seeded PRNG threads the optimizer; a
// whole generation is asked up front, evaluated by a bounded worker pool over
// isolated engine instances, and TOLD BACK IN ASCENDING ID ORDER — so the
// population trajectory (and therefore every artifact) is independent of trial
// completion order. Re-running the same seed reproduces identical trials.
//
// The artifact NUMBER is the optimizer trial id (0..n_trials-1 for a fresh study;
// §6.5). FAILed trials do not join the NSGA-II population (§6.4) but still write
// their FAIL artifact and count toward progress.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/composition"
	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/hyperopt"
	"github.com/byjackchen/trade-tms-go/internal/hyperopt/nsga2"
)

func numCPU() int { return runtime.NumCPU() }

// Sink receives streamed study/trial/progress updates as the study runs. The DB
// store (store.go) and a Redis progress publisher both implement it; a study can
// run with a nil sink (artifact-only mode). All methods must tolerate being
// called from the single coordinator goroutine in order; none are called
// concurrently.
type Sink interface {
	// UpsertStudy writes/updates the study identity + config + live progress.
	UpsertStudy(ctx context.Context, cfg StudyConfig, p Progress) error
	// UpsertTrial writes one completed trial (COMPLETE or FAIL).
	UpsertTrial(ctx context.Context, studyTS string, t TrialArtifact) error
	// Progress writes the live progress snapshot (status, counts, current_best).
	Progress(ctx context.Context, studyTS string, p Progress) error
	// Heartbeat stamps ONLY last_heartbeat_at/updated_at to now (spec §6.10),
	// leaving every other live-progress column untouched. Called by the daemon
	// heartbeat ticker between trial-boundary writes.
	Heartbeat(ctx context.Context, studyTS string, now time.Time) error
}

// Config parameterizes a study run (spec §6.1).
type Config struct {
	Strategy string // sepa|sector_rotation|pairs|joint
	// Composition, when set, is the concrete blueprint a JOINT study targets: its
	// ACTIVE members + weights + risk drive assembly (replacing the static
	// default-multi seed). nil for a single-strategy tune or an older queued joint
	// payload, which falls back to the strategy's seed Composition
	// (docs/concept-alignment.md §3.3).
	Composition     *composition.Composition
	Start, End      calendar.Date // study window (inclusive)
	Population      int           // NSGA-II generation size (default 50)
	Generations     int           // number of generations (>=1)
	Seed            int64         // PRNG seed (default 42)
	Workers         int           // evaluation parallelism (default min(cores-2,16))
	WalkForward     bool          // default true
	Folds           int           // default 5
	EmbargoDays     int           // default 5
	SEPAStocks      []string      // SEPA stock universe (sepa/joint)
	StartingBalance float64       // default 100000
	SPYSymbol       string        // default SPY
	RunsDir         string        // artifact base (default runs/hyperopt)
	StudyTS         string        // pin the study dir / id (default now-UTC)
	// TrialTimeout bounds a single trial's backtest evaluation (spec §5.4/§5.5,
	// §6.1 trial_timeout_sec). Zero => default 600s; a negative value disables
	// the per-trial deadline (CLI 0 maps here to disabled). On timeout the trial
	// FAILs with error "timeout: trial timeout after <N>s".
	TrialTimeout time.Duration
	// Resume, when true with a pinned StudyTS, resumes an existing study: the
	// resume-mismatch guard runs (§6.3), COMPLETE trials are skipped (§6.5), and
	// the optimizer population trajectory is restored by replaying their stored
	// objective values (§6.4) so completed work is not re-executed. Requires a
	// ResumeSource.
	Resume bool
	// ResumeSource loads the prior study identity (§6.3 guard) and its COMPLETE
	// trials (§6.5 replay). Required when Resume is true; the DB store implements
	// it. nil disables resume regardless of the Resume flag.
	ResumeSource ResumeSource
	// Dataset, when set, is the pre-loaded shared dataset (tests / in-process
	// reuse). When nil the caller must supply Feed for LoadDataset.
	Dataset *Dataset
}

// ResumeSource supplies the prior study's identity and completed trials so a
// resumed run can validate compatibility (§6.3) and replay finished work (§6.5).
// The DB Store implements it.
type ResumeSource interface {
	// Get returns the existing study row (its config feeds the §6.3 mismatch
	// guard), or ErrStudyNotFound when there is nothing to resume.
	Get(ctx context.Context, studyTS string) (*StudyRow, error)
	// CompletedTrials returns the study's COMPLETE trials keyed by artifact
	// number, whose stored objective values are replayed instead of re-run.
	CompletedTrials(ctx context.Context, studyTS string) (map[int]CompletedTrial, error)
}

// Result is the outcome of a completed study (§6.9).
type Result struct {
	StudyName string
	StudyTS   string
	StudyDir  string
	Pareto    []nsga2.Solution
}

// objectiveSpecs is the fixed (sharpe, calmar) maximize/maximize objective set
// (§6.4/§12).
func objectiveSpecs() []nsga2.ObjectiveSpec {
	return []nsga2.ObjectiveSpec{
		{Name: "sharpe", Maximize: true},
		{Name: "calmar", Maximize: true},
	}
}

// objectiveEvaluator scores one decoded candidate over the study folds. The
// concrete *Evaluator implements it; tests inject blocking/counting stubs.
type objectiveEvaluator interface {
	Evaluate(ctx context.Context, dec Decoded) (EvalResult, error)
}

// Coordinator owns one study run: the optimizer, evaluator, sink, artifact dir
// and live progress accounting.
type Coordinator struct {
	cfg     Config
	space   *SpaceBuilder
	eval    objectiveEvaluator
	opt     *nsga2.Optimizer
	sink    Sink
	dir     string
	studyTS string
	name    string
	now     func() time.Time

	// progress accounting (in-memory, O(1) — the reference's O(trials²) re-scan
	// is the IMPROVE the spec permits §6.8, output identical).
	mu        sync.Mutex
	completed int
	failed    int
	running   int
	startedAt time.Time
	createdAt time.Time
	best      *CurrentBest
	bestTrial *TrialArtifact // the COMPLETE trial backing current_best (Optuna trial)
	pid       int

	// progressMu serializes every progress.json write (full trial-boundary
	// writes AND the daemon heartbeat's last_heartbeat_at-only rewrite) so the
	// two never tear (§6.10: atomic tmp+rename, last-write-wins).
	progressMu       sync.Mutex
	lastHeartbeat    time.Time // last stamped heartbeat instant (under progressMu)
	trialTimeoutSecs int       // 0 disables; mirrors cfg.TrialTimeout for FAIL text

	// resumeDone maps an already-COMPLETE trial's artifact number to its stored
	// objective values (§6.5). Populated by prepareResume; nil for a fresh study.
	// A trial whose number is present is NOT re-run — its stored values are told
	// to the optimizer (restoring the exact population trajectory) and its
	// already-persisted artifact is left untouched.
	resumeDone map[int][]float64
}

// defaultWorkers returns min(cores-2, 16), floored at 1 (spec parallelism
// default; 16 cores available per the build brief).
func defaultWorkers() int {
	n := numCPU() - 2
	if n > 16 {
		n = 16
	}
	if n < 1 {
		n = 1
	}
	return n
}

// NewCoordinator validates cfg, builds the search space, optimizer and evaluator,
// and prepares the artifact directory + study identity. The shared dataset must
// be supplied via cfg.Dataset (loaded once by the caller).
func NewCoordinator(cfg Config, sink Sink) (*Coordinator, error) {
	if err := validateConfig(&cfg); err != nil {
		return nil, err
	}
	space, err := NewSpaceBuilder(cfg.Strategy)
	if err != nil {
		return nil, err
	}
	if cfg.Dataset == nil {
		return nil, errors.New("hyperopt: coordinator needs a pre-loaded Dataset")
	}

	// Resolve baseline defaults per sub-strategy (for override merging).
	defaults := make(map[string]map[string]any, len(space.order))
	for _, sub := range space.order {
		sp, err := hyperopt.LoadBaselineParams(sub)
		if err != nil {
			return nil, err
		}
		m, err := hyperopt.DefaultsDict(sp)
		if err != nil {
			return nil, fmt.Errorf("hyperopt: defaults for %s: %w", sub, err)
		}
		defaults[sub] = m
	}

	// Walk-forward folds, computed once (§3.2).
	var folds []hyperopt.EvalSegment
	if cfg.WalkForward {
		folds, err = hyperopt.ExpandingAnchored(
			midnight(cfg.Start), midnight(cfg.End), cfg.Folds, cfg.EmbargoDays)
		if err != nil {
			return nil, fmt.Errorf("hyperopt: walk-forward split: %w", err)
		}
	}

	eval, err := NewEvaluator(EvaluatorConfig{
		Strategy:        cfg.Strategy,
		Composition:     cfg.Composition,
		Dataset:         cfg.Dataset,
		Start:           cfg.Start,
		End:             cfg.End,
		Folds:           folds,
		Defaults:        defaults,
		SEPAStocks:      cfg.SEPAStocks,
		StartingBalance: cfg.StartingBalance,
		SPYSymbol:       cfg.SPYSymbol,
	})
	if err != nil {
		return nil, err
	}

	opt, err := nsga2.New(nsga2.Config{
		Space:          space.Space(),
		Objectives:     objectiveSpecs(),
		PopulationSize: cfg.Population,
		Seed:           uint64(cfg.Seed),
	})
	if err != nil {
		return nil, err
	}

	now := time.Now
	studyTS := cfg.StudyTS
	if studyTS == "" {
		studyTS = NewStudyTS(now())
	}
	dir := filepath.Join(cfg.RunsDir, studyTS)
	// Per-trial timeout in whole seconds for the FAIL message (§5.4); <=0 disables.
	timeoutSecs := 0
	if cfg.TrialTimeout > 0 {
		timeoutSecs = int(cfg.TrialTimeout / time.Second)
	}
	return &Coordinator{
		cfg:              cfg,
		space:            space,
		eval:             eval,
		opt:              opt,
		sink:             sink,
		dir:              dir,
		studyTS:          studyTS,
		name:             fmt.Sprintf("hyperopt-%s-%s", cfg.Strategy, studyTS),
		now:              now,
		pid:              os.Getpid(),
		trialTimeoutSecs: timeoutSecs,
	}, nil
}

// StudyTS returns the study timestamp / directory name.
func (c *Coordinator) StudyTS() string { return c.studyTS }

// StudyDir returns the artifact directory path.
func (c *Coordinator) StudyDir() string { return c.dir }

// Run executes the study: writes the initial RUNNING progress + study.json,
// drives the NSGA-II generations, streams artifacts/DB/progress per completion,
// and on normal finish writes COMPLETE + best_params. ctx cancellation writes
// INTERRUPTED and returns ctx.Err(); a fatal error writes INTERRUPTED with the
// error. No goroutine outlives Run.
func (c *Coordinator) Run(ctx context.Context) (*Result, error) {
	c.createdAt = c.now().UTC()
	c.startedAt = c.now().UTC()

	// Resume (§6.2-§6.5): validate compatibility, preserve created_at/started_at,
	// and load the COMPLETE trials to replay. Runs before any state mutation so a
	// mismatch aborts cleanly.
	if err := c.prepareResume(ctx); err != nil {
		return nil, err
	}

	if err := os.MkdirAll(filepath.Join(c.dir, "trials"), 0o755); err != nil {
		return nil, fmt.Errorf("hyperopt: mkdir study dir: %w", err)
	}
	cfgArtifact := c.studyConfig()
	if err := WriteStudyJSON(c.dir, cfgArtifact); err != nil {
		return nil, err
	}
	// Initial RUNNING progress written before any dispatch (§6.8 write point 1),
	// so an early interrupt still leaves a flippable progress file.
	if err := c.writeProgress(ctx, StatusRunning, nil); err != nil {
		return nil, err
	}
	if c.sink != nil {
		if err := c.sink.UpsertStudy(ctx, cfgArtifact, c.snapshot(StatusRunning, nil)); err != nil {
			return nil, fmt.Errorf("hyperopt: sink upsert study: %w", err)
		}
	}

	// Daemon heartbeat (§6.10): started right after the initial RUNNING write,
	// cancelled in defer (blocks until the goroutine exits — no leak).
	stopHeartbeat := c.startHeartbeat(ctx)
	defer stopHeartbeat()

	runErr := c.runGenerations(ctx)
	if runErr != nil {
		// Cancellation or fatal error -> INTERRUPTED (§6.9).
		var lastErr *string
		if !errors.Is(runErr, context.Canceled) && !errors.Is(runErr, context.DeadlineExceeded) {
			s := runErr.Error()
			lastErr = &s
		}
		// Best-effort terminal progress (use Background so a cancelled ctx still
		// flips the file).
		termCtx := context.Background()
		_ = c.writeProgress(termCtx, StatusInterrupted, lastErr)
		if c.sink != nil {
			_ = c.sink.Progress(termCtx, c.studyTS, c.snapshot(StatusInterrupted, lastErr))
		}
		return nil, runErr
	}

	// Normal completion: COMPLETE then best_params (§6.9).
	if err := c.writeProgress(ctx, StatusComplete, nil); err != nil {
		return nil, err
	}
	if c.sink != nil {
		if err := c.sink.Progress(ctx, c.studyTS, c.snapshot(StatusComplete, nil)); err != nil {
			return nil, err
		}
	}
	if err := c.writeBestParams(ctx); err != nil {
		return nil, err
	}
	return &Result{
		StudyName: c.name,
		StudyTS:   c.studyTS,
		StudyDir:  c.dir,
		Pareto:    c.paretoSolutions(),
	}, nil
}

// runGenerations drives the ask/evaluate/tell loop for every generation. Each
// generation is asked up front, evaluated by a bounded worker pool, then told
// back in ascending id order (determinism). Per told trial it writes the
// artifact + DB row + progress.
func (c *Coordinator) runGenerations(ctx context.Context) error {
	parallelism := c.cfg.Workers
	for g := 0; g < c.cfg.Generations; g++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Ask the whole generation (single coordinator goroutine).
		var trials []nsga2.Trial
		for {
			t, ok := c.opt.Ask()
			if !ok {
				break
			}
			trials = append(trials, t)
		}
		c.setRunning(len(trials))
		if err := c.writeProgress(ctx, StatusRunning, nil); err != nil {
			return err
		}

		results := make([]trialOutcome, len(trials))
		if err := c.evaluateGeneration(ctx, trials, results, parallelism); err != nil {
			return err // ctx cancellation
		}

		// Tell in ask (ascending id) order, streaming artifacts deterministically.
		// A replayed (resumed-COMPLETE) trial is told its stored values to rebuild
		// the population but is NOT re-counted nor re-written (§6.5).
		for i, t := range trials {
			out := results[i]
			if err := c.opt.Tell(t, out.values, out.evalErr); err != nil {
				return err
			}
			if out.replayed {
				continue
			}
			c.recordCompletion(out)
			if err := c.streamTrial(ctx, out); err != nil {
				return err
			}
		}
		c.setRunning(0)
		if err := c.writeProgress(ctx, StatusRunning, nil); err != nil {
			return err
		}
		if c.sink != nil {
			if err := c.sink.Progress(ctx, c.studyTS, c.snapshot(StatusRunning, nil)); err != nil {
				return err
			}
		}
	}
	return nil
}

// trialOutcome carries one evaluated trial's result for tell + artifact writing.
type trialOutcome struct {
	trial      nsga2.Trial
	number     int
	values     []float64
	evalErr    error
	result     EvalResult
	startedAt  time.Time
	finishedAt time.Time
	duration   float64
	// replayed marks an already-COMPLETE trial restored on resume (§6.5): its
	// stored objective values are told to the optimizer to rebuild the
	// population, but it is NOT re-counted in progress nor re-written (the prior
	// artifact / DB row stands).
	replayed bool
}

// evaluateGeneration scores every trial honoring ctx and a worker cap. ctx
// cancellation aborts (returned error); an evaluator error is stored per-trial
// (that trial FAILs, the run continues — §6.4). Bounded memory: results are
// pre-sized; no unbounded buffering.
func (c *Coordinator) evaluateGeneration(ctx context.Context, trials []nsga2.Trial, out []trialOutcome, parallelism int) error {
	if parallelism <= 1 {
		for i, t := range trials {
			if err := ctx.Err(); err != nil {
				return err
			}
			if rep, ok := c.replayOutcome(t); ok {
				out[i] = rep
				continue
			}
			out[i] = c.evalOne(ctx, t)
			if out[i].evalErr != nil && ctx.Err() != nil {
				return ctx.Err()
			}
		}
		return nil
	}
	var (
		wg  sync.WaitGroup
		idx = make(chan int)
	)
	worker := func() {
		defer wg.Done()
		for i := range idx {
			if rep, ok := c.replayOutcome(trials[i]); ok {
				out[i] = rep
				continue
			}
			if ctx.Err() != nil {
				out[i] = trialOutcome{trial: trials[i], number: trials[i].ID, evalErr: ctx.Err()}
				continue
			}
			out[i] = c.evalOne(ctx, trials[i])
		}
	}
	wg.Add(parallelism)
	for w := 0; w < parallelism; w++ {
		go worker()
	}
	for i := range trials {
		if ctx.Err() != nil {
			break
		}
		idx <- i
	}
	close(idx)
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

// replayOutcome returns a replayed outcome for an already-COMPLETE trial on
// resume (§6.5): the stored objective values are told to the optimizer to rebuild
// the population, but the trial is NOT re-counted or re-written. ok is false for a
// fresh trial (or non-resume run), which is evaluated normally.
func (c *Coordinator) replayOutcome(t nsga2.Trial) (trialOutcome, bool) {
	if c.resumeDone == nil {
		return trialOutcome{}, false
	}
	vals, ok := c.resumeDone[t.ID]
	if !ok {
		return trialOutcome{}, false
	}
	return trialOutcome{
		trial:    t,
		number:   t.ID,
		values:   append([]float64(nil), vals...),
		replayed: true,
	}, true
}

// evalOne decodes the candidate, runs the fold backtests under a per-trial
// deadline (§5.4/§5.5), and times the call. A decode/validation/run error
// becomes a FAIL outcome (evalErr set, no values). On the per-trial timeout the
// outcome FAILs with error "timeout: trial timeout after <N>s" (§5.4) and the
// backtest is aborted promptly at the next bar boundary (the engine is ctx-aware,
// cooperative). A cancellation of the PARENT study ctx is NOT a per-trial
// timeout: it surfaces as ctx.Err() so the run aborts -> INTERRUPTED (§6.9).
func (c *Coordinator) evalOne(parent context.Context, t nsga2.Trial) trialOutcome {
	started := c.now().UTC()
	dec, err := c.space.Decode(t.Params)
	if err != nil {
		return c.failOutcome(t, started, err)
	}

	// Per-trial deadline: a child ctx with a timeout (§5.5 IMPROVE). When the
	// timeout is disabled (<=0) the trial runs under the parent ctx only.
	ctx := parent
	var cancel context.CancelFunc
	if c.trialTimeoutSecs > 0 {
		ctx, cancel = context.WithTimeout(parent, time.Duration(c.trialTimeoutSecs)*time.Second)
		defer cancel()
	}

	res, err := c.eval.Evaluate(ctx, dec)
	finished := c.now().UTC()
	dur := finished.Sub(started).Seconds()
	if dur < 0 {
		dur = 0
	}
	if err != nil {
		// A per-trial timeout fired but the PARENT ctx is still live => FAIL with
		// the spec timeout shape. If the parent itself was cancelled, fall through
		// to a plain FAIL carrying ctx.Err(); evaluateGeneration detects the
		// parent cancellation and aborts the run.
		if parent.Err() == nil && ctx.Err() != nil {
			err = c.timeoutError()
		}
		o := c.failOutcome(t, started, err)
		o.finishedAt = finished
		o.duration = dur
		return o
	}
	return trialOutcome{
		trial:      t,
		number:     t.ID,
		values:     res.Objectives(),
		result:     res,
		startedAt:  started,
		finishedAt: finished,
		duration:   dur,
	}
}

// timeoutError returns the §5.4 trial-timeout FAIL error: the message text is
// "timeout: trial timeout after <N>s" — the outer wrapper's "timeout: " prefix
// over the inner "trial timeout after <N>s" (the doubled word is intentional,
// matching the reference workers.py:42,203).
func (c *Coordinator) timeoutError() error {
	return fmt.Errorf("timeout: trial timeout after %ds", c.trialTimeoutSecs)
}

func (c *Coordinator) failOutcome(t nsga2.Trial, started time.Time, err error) trialOutcome {
	fin := c.now().UTC()
	return trialOutcome{
		trial:      t,
		number:     t.ID,
		evalErr:    err,
		startedAt:  started,
		finishedAt: fin,
		duration:   fin.Sub(started).Seconds(),
	}
}

// streamTrial writes one trial's artifact + DB row, then updates progress.
func (c *Coordinator) streamTrial(ctx context.Context, out trialOutcome) error {
	art := c.trialArtifact(out)
	if err := WriteTrialJSON(c.dir, art); err != nil {
		return err
	}
	if c.sink != nil {
		if err := c.sink.UpsertTrial(ctx, c.studyTS, art); err != nil {
			return fmt.Errorf("hyperopt: sink upsert trial %d: %w", art.Number, err)
		}
	}
	return nil
}
