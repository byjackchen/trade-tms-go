package study

// resume.go implements study resume (spec §6.2-§6.5): re-running a pinned
// study_ts that was INTERRUPTED (or partially completed) without re-executing
// already-COMPLETE trials.
//
// Design (locked-decision determinism): the self-written NSGA-II is fully
// deterministic — the same seed + the same per-trial objective values reproduce
// the identical population trajectory. So resume does NOT need to byte-restore
// the PRNG state; it RE-DRIVES the optimizer from the same seed in the same
// ask/tell order, but for any trial whose artifact number is already COMPLETE it
// REPLAYS the stored (sharpe, calmar) objective values instead of re-running the
// backtest. Telling the optimizer those exact values rebuilds the exact same
// population it had on the original run, while the expensive fold backtests are
// skipped (§6.5). FAIL/missing trials are re-run and overwritten.
//
// §6.3 mismatch guard: when resuming, the new invocation's strategy / start /
// end / walk_forward must deep-equal the existing study row's; any mismatch is a
// hard error listing all mismatched fields (seed / n_trials / workers /
// trial_timeout_sec may change and are NOT validated). §6.2: created_at and
// started_at are preserved from the existing row.

import (
	"context"
	"fmt"
	"strings"
)

// prepareResume validates resume compatibility (§6.3), preserves created_at /
// started_at from the existing study (§6.2), and loads the already-COMPLETE
// trials to replay (§6.5). It is a no-op (returns nil) when cfg.Resume is false.
// Called at the top of Run, before any artifact/DB write, so a mismatch aborts
// cleanly without mutating state.
func (c *Coordinator) prepareResume(ctx context.Context) error {
	if !c.cfg.Resume {
		return nil
	}
	if c.cfg.ResumeSource == nil {
		return fmt.Errorf("hyperopt: resume requires a ResumeSource")
	}
	existing, err := c.cfg.ResumeSource.Get(ctx, c.studyTS)
	if err != nil {
		return fmt.Errorf("hyperopt: resume: load study %s: %w", c.studyTS, err)
	}
	if err := c.checkResumeMismatch(existing); err != nil {
		return err
	}
	// §6.2: preserve created_at / started_at from the prior run; keep the prior
	// study_name (it is "hyperopt-<strategy>-<ts>" — identical given the ts).
	if !existing.CreatedAt.IsZero() {
		c.createdAt = existing.CreatedAt.UTC()
	}
	if existing.StartedAt != nil && !existing.StartedAt.IsZero() {
		c.startedAt = existing.StartedAt.UTC()
	}
	if existing.StudyName != "" {
		c.name = existing.StudyName
	}

	done, err := c.cfg.ResumeSource.CompletedTrials(ctx, c.studyTS)
	if err != nil {
		return fmt.Errorf("hyperopt: resume: load completed trials: %w", err)
	}
	c.resumeDone = make(map[int][]float64, len(done))
	for num, ct := range done {
		// Objective order is (sharpe, calmar) — the fixed objective vector (§6.4).
		c.resumeDone[num] = []float64{ct.Sharpe, ct.Calmar}
		// Restore the in-memory current_best / completed counter so progress
		// reflects the prior work immediately (and never regresses).
		c.mu.Lock()
		c.completed++
		score := ct.Sharpe + ct.Calmar
		if c.best == nil || score > c.best.Sharpe+c.best.Calmar {
			c.best = &CurrentBest{Trial: num, Sharpe: ct.Sharpe, Calmar: ct.Calmar}
		}
		c.mu.Unlock()
	}
	return nil
}

// checkResumeMismatch compares the new invocation against the existing study row
// on strategy / start / end / walk_forward (§6.3). Any mismatch is a hard error
// listing every mismatched field joined by "; " (matching the reference message
// shape). seed / n_trials / workers / trial_timeout_sec are intentionally not
// validated (a resume may change them).
func (c *Coordinator) checkResumeMismatch(existing *StudyRow) error {
	var diffs []string
	if existing.Strategy != c.cfg.Strategy {
		diffs = append(diffs, fmt.Sprintf("strategy: existing=%s new=%s", existing.Strategy, c.cfg.Strategy))
	}
	if existing.Start != c.cfg.Start.String() {
		diffs = append(diffs, fmt.Sprintf("start: existing=%s new=%s", existing.Start, c.cfg.Start.String()))
	}
	if existing.End != c.cfg.End.String() {
		diffs = append(diffs, fmt.Sprintf("end: existing=%s new=%s", existing.End, c.cfg.End.String()))
	}
	newWF := WalkForward{Enabled: c.cfg.WalkForward, Folds: c.cfg.Folds, EmbargoDays: c.cfg.EmbargoDays}
	if existing.WalkForward != newWF {
		diffs = append(diffs, fmt.Sprintf("walk_forward: existing=%+v new=%+v", existing.WalkForward, newWF))
	}
	if len(diffs) > 0 {
		return fmt.Errorf("resume mismatch for study %s: %s", c.studyTS, strings.Join(diffs, "; "))
	}
	return nil
}
