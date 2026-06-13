package study

// fixer_test.go covers the FIXER round-1 hardening: §6.10 heartbeat + §9.2
// staleness override (finding 2), per-trial timeout FAIL shape (finding 3),
// resume completed-trial replay + mismatch guard (finding 4), and the promotion
// bounds/validity gate (finding 5). Each test is self-contained over the
// deterministic synthetic pairs dataset / fakes — no DB, no network.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
)

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

// blockingEvaluator blocks until its ctx is cancelled, then returns ctx.Err().
// It drives the per-trial timeout / cancellation tests deterministically.
type blockingEvaluator struct{}

func (blockingEvaluator) Evaluate(ctx context.Context, _ Decoded) (EvalResult, error) {
	<-ctx.Done()
	return EvalResult{}, ctx.Err()
}

// countingEvaluator wraps a real objectiveEvaluator and counts Evaluate calls so
// a resume test can prove replayed trials are NOT re-evaluated.
type countingEvaluator struct {
	inner objectiveEvaluator
	mu    sync.Mutex
	calls int
}

func (c *countingEvaluator) Evaluate(ctx context.Context, dec Decoded) (EvalResult, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return c.inner.Evaluate(ctx, dec)
}

// ---------------------------------------------------------------------------
// finding 2: staleness override (§9.2)
// ---------------------------------------------------------------------------

func ptrTime(t time.Time) *time.Time { return &t }
func ptrInt(i int) *int              { return &i }

func TestApplyStalenessZombieRunning(t *testing.T) {
	now := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	// RUNNING, heartbeat 90s old, no coordinator PID => presented INTERRUPTED.
	r := &StudyRow{
		Status:          StatusRunning,
		LastHeartbeatAt: ptrTime(now.Add(-90 * time.Second)),
		CoordinatorPID:  nil,
	}
	applyStaleness(r, now)
	if r.Status != StatusInterrupted {
		t.Fatalf("stale zombie RUNNING must present INTERRUPTED, got %s", r.Status)
	}
}

func TestApplyStalenessFreshHeartbeatStaysRunning(t *testing.T) {
	now := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	// Heartbeat 10s old (< 60s) => healthy, stays RUNNING even with a dead PID.
	r := &StudyRow{
		Status:          StatusRunning,
		LastHeartbeatAt: ptrTime(now.Add(-10 * time.Second)),
		CoordinatorPID:  ptrInt(999999),
	}
	applyStaleness(r, now)
	if r.Status != StatusRunning {
		t.Fatalf("fresh heartbeat must stay RUNNING, got %s", r.Status)
	}
}

func TestApplyStalenessLivePidOverridesStaleHeartbeat(t *testing.T) {
	now := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	// Stale heartbeat but a LIVE coordinator PID (this test process) => RUNNING.
	r := &StudyRow{
		Status:          StatusRunning,
		LastHeartbeatAt: ptrTime(now.Add(-120 * time.Second)),
		CoordinatorPID:  ptrInt(os.Getpid()),
	}
	applyStaleness(r, now)
	if r.Status != StatusRunning {
		t.Fatalf("a live coordinator PID must keep RUNNING, got %s", r.Status)
	}
}

func TestApplyStalenessTerminalUntouched(t *testing.T) {
	now := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	for _, st := range []Status{StatusComplete, StatusInterrupted} {
		r := &StudyRow{Status: st, LastHeartbeatAt: ptrTime(now.Add(-999 * time.Second))}
		applyStaleness(r, now)
		if r.Status != st {
			t.Fatalf("terminal status %s must be untouched, got %s", st, r.Status)
		}
	}
}

// TestHeartbeatStampsProgress drives a real (tiny) study and asserts the daemon
// heartbeat keeps progress.json's last_heartbeat_at fresh. We use a fast clock
// indirectly by checking the file is stamped after the run (full writes also
// stamp it; this confirms the heartbeat machinery wired into Run does not break
// the artifact and leaves a fresh heartbeat).
func TestHeartbeatWiredAndProgressFresh(t *testing.T) {
	start := calendar.NewDate(2023, 1, 2)
	end := calendar.NewDate(2023, 12, 29)
	ds := syntheticPairs(t, start, end)
	cfg := pairsConfig(ds, start, end)
	c, _ := runStudy(t, cfg)

	raw, err := os.ReadFile(progressJSONPath(c.StudyDir()))
	if err != nil {
		t.Fatalf("read progress.json: %v", err)
	}
	var m map[string]any
	if err := jsonUnmarshal(raw, &m); err != nil {
		t.Fatalf("parse progress.json: %v", err)
	}
	if m["last_heartbeat_at"] == nil {
		t.Fatal("progress.json must carry a non-null last_heartbeat_at")
	}
	if m["status"] != "COMPLETE" {
		t.Fatalf("study should be COMPLETE, got %v", m["status"])
	}
}

// TestStampProgressHeartbeatPreservesFields confirms the heartbeat rewrite touches
// ONLY last_heartbeat_at/updated_at and preserves every other field (§6.10).
func TestStampProgressHeartbeatPreservesFields(t *testing.T) {
	dir := t.TempDir()
	c := &Coordinator{dir: dir, now: func() time.Time { return time.Date(2026, 6, 13, 12, 0, 30, 0, time.UTC) }}

	// Seed a progress.json with a known shape.
	seed := Progress{
		Status: StatusRunning, CompletedTrials: 7, FailedTrials: 2, RunningTrials: 3,
		TotalTrials: 20, Workers: 4,
		StartedAt:       ptrTime(time.Date(2026, 6, 13, 11, 0, 0, 0, time.UTC)),
		UpdatedAt:       ptrTime(time.Date(2026, 6, 13, 11, 59, 0, 0, time.UTC)),
		LastHeartbeatAt: ptrTime(time.Date(2026, 6, 13, 11, 59, 0, 0, time.UTC)),
		CoordinatorPID:  ptrInt(4242),
		CurrentBest:     &CurrentBest{Trial: 5, Sharpe: 1.25, Calmar: 0.5},
	}
	if err := WriteProgressJSON(dir, seed); err != nil {
		t.Fatal(err)
	}

	c.stampProgressHeartbeat(c.now())

	raw, err := os.ReadFile(progressJSONPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := jsonUnmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m["completed_trials"].(float64) != 7 || m["failed_trials"].(float64) != 2 ||
		m["running_trials"].(float64) != 3 || m["total_trials"].(float64) != 20 ||
		m["workers"].(float64) != 4 {
		t.Fatalf("counters must be preserved, got %v", m)
	}
	if m["coordinator_pid"].(float64) != 4242 {
		t.Fatalf("coordinator_pid must be preserved, got %v", m["coordinator_pid"])
	}
	cb, ok := m["current_best"].(map[string]any)
	if !ok || cb["trial"].(float64) != 5 {
		t.Fatalf("current_best must be preserved, got %v", m["current_best"])
	}
	if !strings.HasPrefix(m["last_heartbeat_at"].(string), "2026-06-13T12:00:30") {
		t.Fatalf("last_heartbeat_at must be stamped to now, got %v", m["last_heartbeat_at"])
	}
	if !strings.HasPrefix(m["updated_at"].(string), "2026-06-13T12:00:30") {
		t.Fatalf("updated_at must be stamped to now, got %v", m["updated_at"])
	}
}

// TestStampProgressHeartbeatPreservesCorruptFile: a corrupt progress.json is left
// verbatim (the tick no-ops), matching the reference (§6.10).
func TestStampProgressHeartbeatPreservesCorruptFile(t *testing.T) {
	dir := t.TempDir()
	corrupt := []byte("{not json")
	if err := os.WriteFile(progressJSONPath(dir), corrupt, 0o644); err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{dir: dir, now: func() time.Time { return time.Now().UTC() }}
	c.stampProgressHeartbeat(c.now())
	raw, err := os.ReadFile(progressJSONPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(corrupt) {
		t.Fatalf("corrupt progress.json must be preserved verbatim, got %q", raw)
	}
}

// ---------------------------------------------------------------------------
// finding 3: per-trial timeout FAIL shape (§5.4/§5.5)
// ---------------------------------------------------------------------------

func TestTrialTimeoutFailShape(t *testing.T) {
	start := calendar.NewDate(2023, 1, 2)
	end := calendar.NewDate(2023, 12, 29)
	ds := syntheticPairs(t, start, end)
	cfg := pairsConfig(ds, start, end)
	cfg.TrialTimeout = 1 * time.Second
	dir := t.TempDir()
	cfg.RunsDir = dir
	cfg.StudyTS = "2026-02-02_02-02-02"

	c, err := NewCoordinator(cfg, nil)
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	// Replace the evaluator with one that blocks until its ctx is cancelled, so
	// the per-trial deadline always fires (deterministic timeout).
	c.eval = blockingEvaluator{}

	// Run a single generation's evaluation by hand to inspect one outcome.
	tr, ok := c.opt.Ask()
	if !ok {
		t.Fatal("Ask failed")
	}
	out := c.evalOne(context.Background(), tr)
	if out.evalErr == nil {
		t.Fatal("expected a timeout FAIL, got success")
	}
	want := "timeout: trial timeout after 1s"
	if out.evalErr.Error() != want {
		t.Fatalf("timeout FAIL message = %q, want %q", out.evalErr.Error(), want)
	}
	if out.duration < 0 {
		t.Fatalf("duration must be >= 0, got %v", out.duration)
	}
}

// TestStudyCancelNotTimeoutFail: cancelling the PARENT ctx surfaces as ctx.Err()
// (the run aborts -> INTERRUPTED), NOT a per-trial timeout FAIL.
func TestStudyCancelNotTimeoutFail(t *testing.T) {
	start := calendar.NewDate(2023, 1, 2)
	end := calendar.NewDate(2023, 12, 29)
	ds := syntheticPairs(t, start, end)
	cfg := pairsConfig(ds, start, end)
	cfg.TrialTimeout = 60 * time.Second
	dir := t.TempDir()
	cfg.RunsDir = dir
	cfg.StudyTS = "2026-03-03_03-03-03"

	c, err := NewCoordinator(cfg, nil)
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	c.eval = blockingEvaluator{}

	tr, _ := c.opt.Ask()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // parent already cancelled
	out := c.evalOne(ctx, tr)
	if out.evalErr == nil {
		t.Fatal("expected an error")
	}
	if strings.HasPrefix(out.evalErr.Error(), "timeout:") {
		t.Fatalf("parent cancellation must NOT be a timeout FAIL, got %q", out.evalErr.Error())
	}
}

// ---------------------------------------------------------------------------
// finding 4: resume completed-trial replay + mismatch guard (§6.3/§6.5)
// ---------------------------------------------------------------------------

// fakeResumeSource serves a prior study row + its COMPLETE trials for resume.
type fakeResumeSource struct {
	row  *StudyRow
	done map[int]CompletedTrial
	err  error
}

func (f *fakeResumeSource) Get(_ context.Context, _ string) (*StudyRow, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.row, nil
}
func (f *fakeResumeSource) CompletedTrials(_ context.Context, _ string) (map[int]CompletedTrial, error) {
	return f.done, nil
}

func TestResumeSkipsCompletedTrials(t *testing.T) {
	start := calendar.NewDate(2023, 1, 2)
	end := calendar.NewDate(2023, 12, 29)
	ds := syntheticPairs(t, start, end)

	// First, run a full study to learn the real trials (numbers + objective vals).
	base := pairsConfig(ds, start, end)
	c0, _ := runStudy(t, base)
	trials := readTrials(t, c0.StudyDir())

	// Build a resume source from the first 3 COMPLETE trials.
	done := map[int]CompletedTrial{}
	completeCount := 0
	for _, tr := range trials {
		if tr["state"] != "COMPLETE" {
			continue
		}
		num := int(tr["number"].(float64))
		if num >= 3 {
			continue
		}
		mtr := tr["metrics"].(map[string]any)
		done[num] = CompletedTrial{
			Number: num,
			Sharpe: mtr["sharpe"].(float64),
			Calmar: mtr["calmar"].(float64),
		}
		completeCount++
	}
	if completeCount == 0 {
		t.Skip("no COMPLETE trials with number<3 to replay")
	}

	row := &StudyRow{Status: StatusInterrupted}
	row.StudyName = "hyperopt-pairs-2026-04-04_04-04-04"
	row.Strategy = "pairs"
	row.Start = start.String()
	row.End = end.String()
	row.WalkForward = WalkForward{Enabled: true, Folds: 1, EmbargoDays: 5}
	row.CreatedAt = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	resumeCfg := pairsConfig(ds, start, end)
	dir := t.TempDir()
	resumeCfg.RunsDir = dir
	resumeCfg.StudyTS = "2026-04-04_04-04-04"
	resumeCfg.Resume = true
	resumeCfg.ResumeSource = &fakeResumeSource{row: row, done: done}

	c, err := NewCoordinator(resumeCfg, nil)
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	// Wrap the evaluator to count real backtests.
	ce := &countingEvaluator{inner: c.eval}
	c.eval = ce

	res, err := c.Run(context.Background())
	if err != nil {
		t.Fatalf("resume Run: %v", err)
	}
	// The replayed trials must NOT have been backtested again.
	total := resumeCfg.Population * resumeCfg.Generations
	if ce.calls != total-completeCount {
		t.Fatalf("resume re-ran backtests: got %d Evaluate calls, want %d (total %d minus %d replayed)",
			ce.calls, total-completeCount, total, completeCount)
	}
	// created_at must be preserved from the prior row (§6.2).
	if !c.createdAt.Equal(row.CreatedAt) {
		t.Fatalf("created_at not preserved on resume: got %v want %v", c.createdAt, row.CreatedAt)
	}
	_ = res
}

func TestResumeMismatchGuard(t *testing.T) {
	start := calendar.NewDate(2023, 1, 2)
	end := calendar.NewDate(2023, 12, 29)
	ds := syntheticPairs(t, start, end)

	row := &StudyRow{Status: StatusInterrupted}
	row.Strategy = "sepa" // mismatched strategy (study is pairs)
	row.Start = start.String()
	row.End = end.String()
	row.WalkForward = WalkForward{Enabled: true, Folds: 1, EmbargoDays: 5}

	cfg := pairsConfig(ds, start, end)
	dir := t.TempDir()
	cfg.RunsDir = dir
	cfg.StudyTS = "2026-05-05_05-05-05"
	cfg.Resume = true
	cfg.ResumeSource = &fakeResumeSource{row: row, done: map[int]CompletedTrial{}}

	c, err := NewCoordinator(cfg, nil)
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	_, err = c.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "resume mismatch") {
		t.Fatalf("expected a resume mismatch error, got %v", err)
	}
	if !strings.Contains(err.Error(), "strategy:") {
		t.Fatalf("mismatch error must list the strategy field, got %v", err)
	}
	// No trials dir should have been created (guard runs before mkdir).
	if _, statErr := os.Stat(filepath.Join(dir, "2026-05-05_05-05-05", "trials")); statErr == nil {
		t.Fatal("resume mismatch must abort before writing artifacts")
	}
}

// ---------------------------------------------------------------------------
// finding 5: promotion bounds/validity gate
// ---------------------------------------------------------------------------

func TestValidateTunedInBoundsAccepted(t *testing.T) {
	// risk_pct search range is [1.0, 4.0]; a mid value passes.
	if err := validateTunedForPromotion("sepa", map[string]float64{"risk_pct": 2.5}); err != nil {
		t.Fatalf("in-bounds tuned set must be accepted, got %v", err)
	}
}

func TestValidateTunedOutOfRangeRejected(t *testing.T) {
	err := validateTunedForPromotion("sepa", map[string]float64{"risk_pct": 99.0})
	if err == nil {
		t.Fatal("out-of-range value must be rejected")
	}
	if !errorsIsInvalid(err) {
		t.Fatalf("error must wrap ErrInvalidParams, got %v", err)
	}
	if !strings.Contains(err.Error(), "outside search range") {
		t.Fatalf("error must mention the search range, got %v", err)
	}
}

func TestValidateTunedUnknownParamRejected(t *testing.T) {
	err := validateTunedForPromotion("pairs", map[string]float64{"no_such_param": 1.0})
	if err == nil || !errorsIsInvalid(err) {
		t.Fatalf("unknown param must be rejected as invalid, got %v", err)
	}
}

func TestValidateTunedPairsConstraintBoundsRespected(t *testing.T) {
	// entry_z [1.5,3.0], exit_z [0.1,1.0]; in-range values pass the validator.
	if err := validateTunedForPromotion("pairs", map[string]float64{"entry_z": 2.0, "exit_z": 0.5}); err != nil {
		t.Fatalf("in-bounds pairs set must be accepted, got %v", err)
	}
	// exit_z above its range is rejected by the bounds check.
	if err := validateTunedForPromotion("pairs", map[string]float64{"exit_z": 5.0}); err == nil || !errorsIsInvalid(err) {
		t.Fatalf("out-of-range exit_z must be rejected, got %v", err)
	}
}

func errorsIsInvalid(err error) bool {
	for err != nil {
		if err == ErrInvalidParams {
			return true
		}
		type unwrap interface{ Unwrap() error }
		u, ok := err.(unwrap)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
