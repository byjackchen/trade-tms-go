package study

// coordinator_state.go holds the coordinator's progress accounting, artifact
// projection (outcome -> TrialArtifact), current_best computation, best_params
// promotion (§8.1), config validation/defaulting, and the snapshot helpers the
// progress writers consume. The accounting is in-memory and O(1) per completion
// (the IMPROVE the spec §6.8 permits — the reference re-scans all artifacts each
// write; the output current_best is identical: argmax of sharpe+calmar over
// COMPLETE trials, first-seen wins ties, scan order = ascending trial id).

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/composition"
	"github.com/byjackchen/trade-tms-go/internal/hyperopt/nsga2"
	"github.com/byjackchen/trade-tms-go/internal/metrics"
)

const (
	defaultPopulation = 50
	defaultSeed       = 42
	defaultFolds      = 5
	defaultEmbargo    = 5
	defaultBalance    = 100000.0
	defaultRunsDir    = "runs/hyperopt"
	// defaultTrialTimeout is the spec §6.1 trial_timeout_sec default (600s). A
	// Config.TrialTimeout of exactly zero defaults to this; a negative value
	// disables the per-trial deadline (CLI --trial-timeout-sec 0 maps to nil).
	defaultTrialTimeout = 600 * time.Second
	// heartbeatInterval is the daemon liveness tick (spec §6.10 / §12: 20s).
	heartbeatInterval = 20 * time.Second
)

// validateConfig validates and defaults a Config in place (§6.1).
func validateConfig(cfg *Config) error {
	if cfg.Kind == "" {
		cfg.Kind = KindStrategy
	}
	switch cfg.Kind {
	case KindStrategy:
		switch cfg.Strategy {
		case "sepa", "sector_rotation", "pairs", "joint":
		default:
			return fmt.Errorf("unknown strategy: %s", cfg.Strategy)
		}
	case KindComposition:
		// The composition path samples a blueprint, not a strategy. Strategy is a
		// fixed marker token ("composition") used only for the study name/row; the
		// concrete strategies come from the target Composition's active members.
		if cfg.Strategy == "" {
			cfg.Strategy = string(KindComposition)
		}
		if cfg.Composition == nil {
			return fmt.Errorf("hyperopt: composition study needs a target Composition")
		}
	default:
		return fmt.Errorf("hyperopt: unknown study kind %q", cfg.Kind)
	}
	if cfg.Start.IsZero() || cfg.End.IsZero() {
		return fmt.Errorf("hyperopt: study needs start and end dates")
	}
	if !cfg.End.After(cfg.Start) {
		return fmt.Errorf("hyperopt: end %s must be after start %s", cfg.End, cfg.Start)
	}
	if cfg.Generations < 1 {
		return fmt.Errorf("hyperopt: generations must be >= 1")
	}
	if cfg.Population == 0 {
		cfg.Population = defaultPopulation
	}
	if cfg.Population < 2 {
		return fmt.Errorf("hyperopt: population must be >= 2")
	}
	if cfg.Seed == 0 {
		cfg.Seed = defaultSeed
	}
	if cfg.Workers <= 0 {
		cfg.Workers = defaultWorkers()
	}
	if cfg.Folds == 0 {
		cfg.Folds = defaultFolds
	}
	if cfg.EmbargoDays < 0 {
		cfg.EmbargoDays = defaultEmbargo
	}
	if cfg.StartingBalance <= 0 {
		cfg.StartingBalance = defaultBalance
	}
	// Per-trial timeout (§6.1): zero defaults to 600s; a negative value disables
	// the deadline (the CLI maps --trial-timeout-sec 0 to a negative sentinel).
	if cfg.TrialTimeout == 0 {
		cfg.TrialTimeout = defaultTrialTimeout
	}
	if cfg.RunsDir == "" {
		cfg.RunsDir = defaultRunsDir
	}
	if cfg.SPYSymbol == "" {
		cfg.SPYSymbol = "SPY"
	}
	// Any study whose strategy set includes SEPA needs a stock universe to trade:
	// sepa/joint on the strategy path, or an ACTIVE sepa member on the composition
	// path.
	if configWantsSEPA(cfg) && len(cfg.SEPAStocks) == 0 {
		return fmt.Errorf("hyperopt: a study trading SEPA requires SEPAStocks (the stock universe)")
	}
	return nil
}

// configWantsSEPA reports whether a study's strategy set includes SEPA: the
// sepa/joint selector on the strategy path, or an ACTIVE sepa member of the target
// Composition on the composition path.
func configWantsSEPA(cfg *Config) bool {
	if cfg.Kind == KindComposition {
		if cfg.Composition == nil {
			return false
		}
		for _, m := range cfg.Composition.Members {
			if m.Active && m.StrategyID == composition.StrategySEPA {
				return true
			}
		}
		return false
	}
	return cfg.Strategy == "sepa" || cfg.Strategy == "joint"
}

// totalTrials is population * generations.
func (c *Coordinator) totalTrials() int { return c.cfg.Population * c.cfg.Generations }

// studyConfig builds the study.json config (§7.2). UpdatedAt is refreshed each
// call; CreatedAt is fixed at the study start.
func (c *Coordinator) studyConfig() StudyConfig {
	// trial_timeout_sec: whole seconds, or nil when disabled (<=0).
	var timeout *int
	if c.trialTimeoutSecs > 0 {
		t := c.trialTimeoutSecs
		timeout = &t
	}
	kind := c.cfg.Kind
	if kind == "" {
		kind = KindStrategy
	}
	var compID string
	var ranges *CompositionRanges
	if kind == KindComposition && c.cfg.Composition != nil {
		compID = c.cfg.Composition.ID
		r := DefaultCompositionRanges()
		if c.cfg.CompositionRanges != nil {
			r = *c.cfg.CompositionRanges
		}
		ranges = &r
	}
	return StudyConfig{
		Version:       1,
		StudyName:     c.name,
		Strategy:      c.cfg.Strategy,
		Kind:          kind,
		CompositionID: compID,
		SearchConfig:  ranges,
		Start:         c.cfg.Start.String(),
		End:           c.cfg.End.String(),
		Directions: []string{"maximize", "maximize"},
		Objectives: []string{"sharpe", "calmar"},
		Seed:       c.cfg.Seed,
		NTrials:    c.totalTrials(),
		Workers:    c.cfg.Workers,
		WalkForward: WalkForward{
			Enabled:     c.cfg.WalkForward,
			Folds:       c.cfg.Folds,
			EmbargoDays: c.cfg.EmbargoDays,
		},
		CreatedAt:       c.createdAt,
		UpdatedAt:       c.now().UTC(),
		TrialTimeoutSec: timeout,
	}
}

// setRunning sets the running-trials counter (under the lock).
func (c *Coordinator) setRunning(n int) {
	c.mu.Lock()
	c.running = n
	c.mu.Unlock()
}

// recordCompletion folds one finished trial into the counters and updates
// current_best when the trial is COMPLETE and beats the running best (strict >
// on sharpe+calmar; first-seen wins ties because we scan in ascending id order).
func (c *Coordinator) recordCompletion(out trialOutcome) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if out.evalErr != nil {
		c.failed++
		return
	}
	c.completed++
	sharpe := out.result.Aggregated.Sharpe
	calmar := out.result.Aggregated.Calmar
	score := sharpe + calmar
	if c.best == nil || score > c.best.Sharpe+c.best.Calmar {
		c.best = &CurrentBest{Trial: out.number, Sharpe: sharpe, Calmar: calmar}
	}
}

// snapshot builds a Progress value for status with the current counters. lastErr
// is nil for non-error writes. Timestamps are now-UTC.
func (c *Coordinator) snapshot(status Status, lastErr *string) Progress {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now().UTC()
	started := c.startedAt
	pid := c.pid
	var best *CurrentBest
	if c.best != nil {
		b := *c.best
		best = &b
	}
	return Progress{
		Status:          status,
		CompletedTrials: c.completed,
		FailedTrials:    c.failed,
		RunningTrials:   c.running,
		TotalTrials:     c.totalTrials(),
		Workers:         c.cfg.Workers,
		StartedAt:       &started,
		UpdatedAt:       &now,
		LastHeartbeatAt: &now,
		CoordinatorPID:  &pid,
		CurrentBest:     best,
		LastError:       lastErr,
	}
}

// writeProgress writes progress.json for status (§6.8). The sink progress write
// is driven separately by the caller at generation boundaries. The write is
// serialized with the daemon heartbeat under progressMu so the file never tears
// (§6.10); a full write counts as a heartbeat (it stamps last_heartbeat_at).
func (c *Coordinator) writeProgress(_ context.Context, status Status, lastErr *string) error {
	snap := c.snapshot(status, lastErr)
	c.progressMu.Lock()
	defer c.progressMu.Unlock()
	if snap.UpdatedAt != nil {
		c.lastHeartbeat = *snap.UpdatedAt
	}
	return WriteProgressJSON(c.dir, snap)
}

// trialArtifact projects a finished outcome into a TrialArtifact (§7.4). For
// joint studies the params are nested per sub-strategy; otherwise flat.
func (c *Coordinator) trialArtifact(out trialOutcome) TrialArtifact {
	num := out.number
	optuna := num
	art := TrialArtifact{
		Number:       num,
		OptunaNumber: &optuna,
		Strategy:     c.cfg.Strategy,
		StartedAt:    out.startedAt,
		DurationS:    out.duration,
	}
	if !out.finishedAt.IsZero() {
		fin := out.finishedAt
		art.FinishedAt = &fin
	}
	if out.evalErr != nil {
		art.State = TrialFail
		msg := out.evalErr.Error()
		art.Error = &msg
		art.Params = c.recordedParams(out.trial.Params)
		art.Metrics = metrics.BacktestMetrics{}
		return art
	}
	art.State = TrialComplete
	art.Params = c.recordedParams(out.trial.Params)
	art.Metrics = out.result.Aggregated
	for i, m := range out.result.FoldMetrics {
		art.Folds = append(art.Folds, FoldMetric{Fold: i, Metrics: m})
	}
	return art
}

// recordedParams renders the optimizer's decoded candidate into the artifact
// params map. The projection is kind-specific (strategy: flat/nested signal
// params; composition: the normalized blueprint values) and owned by the
// studySpace.
func (c *Coordinator) recordedParams(cand nsga2.Params) map[string]any {
	return c.space.RecordedParams(cand)
}

func stripPrefix(full, sub string) string {
	p := sub + "."
	if len(full) > len(p) && full[:len(p)] == p {
		return full[len(p):]
	}
	return full
}

// ---------------------------------------------------------------------------
// best_params promotion (§8.1)
// ---------------------------------------------------------------------------

// paretoSolutions returns the rank-0 (non-dominated) COMPLETE solutions of the
// final population, sorted by id ascending.
func (c *Coordinator) paretoSolutions() []nsga2.Solution {
	pop := c.opt.PopulationSolutions()
	var front []nsga2.Solution
	for _, s := range pop {
		if s.Rank == 0 {
			front = append(front, s)
		}
	}
	sort.SliceStable(front, func(a, b int) bool { return front[a].ID < front[b].ID })
	return front
}

// writeBestParams writes best_params/<strat>.json after COMPLETE (§8.1). It picks
// the Pareto-optimal COMPLETE trial with the highest sharpe (values[0]; first on
// ties, ascending id order), then for each sub-strategy tunes the package
// baseline with that trial's pre-clamp params. An empty Pareto front silently
// skips (no best_params dir), matching the reference.
func (c *Coordinator) writeBestParams(_ context.Context) error {
	// A composition study tunes a blueprint, not strategy SIGNAL params: there is no
	// best_params/<strat>.json to write (promotion overwrites the Composition IN
	// PLACE — promote.go). Skip cleanly.
	sb, ok := c.space.(*SpaceBuilder)
	if !ok {
		return nil
	}
	front := c.paretoSolutions()
	if len(front) == 0 {
		return nil
	}
	// argmax over sharpe (values[0]); first maximal on ties (ascending id).
	best := front[0]
	for _, s := range front[1:] {
		if len(s.Values) > 0 && len(best.Values) > 0 && s.Values[0] > best.Values[0] {
			best = s
		}
	}
	strategies := sb.order // single element, or the joint triple
	now := c.now().UTC()
	for _, sub := range strategies {
		tuned := filterPrefixed(best.Params, sub)
		if len(tuned) == 0 {
			continue
		}
		body, err := TuneBaseline(TuneInput{
			Strategy:    sub,
			Tuned:       tuned,
			StudyName:   c.name,
			TrialNumber: best.ID,
			Now:         now,
		})
		if err != nil {
			return fmt.Errorf("hyperopt: tuning best_params for %s: %w", sub, err)
		}
		if err := WriteBestParamsJSON(c.dir, sub, body); err != nil {
			return err
		}
	}
	return nil
}

// filterPrefixed extracts the sub-strategy's params from a candidate's decoded
// map (keyed by PREFIXED name), stripping the prefix and coercing to float64
// (the promoted-default value).
func filterPrefixed(cand nsga2.Params, sub string) map[string]float64 {
	out := map[string]float64{}
	p := sub + "."
	for full, v := range cand {
		if len(full) <= len(p) || full[:len(p)] != p {
			continue
		}
		f, err := toFloat(v)
		if err != nil {
			continue
		}
		out[full[len(p):]] = f
	}
	return out
}
