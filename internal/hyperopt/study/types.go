package study

// types.go declares the study coordinator's public configuration and the
// artifact-shaped value types (StudyConfig, Progress, TrialArtifact) that mirror
// study.json / progress.json / trial_%04d.json (spec §7). The same structs feed
// both the legacy JSON artifact writer (artifacts.go, byte-compatible with the
// Python schemas) and the DB store (store.go).

import (
	"time"

	"github.com/byjackchen/trade-tms-go/internal/metrics"
)

// Status is the coordinator-written lifecycle vocabulary (§6.9): RUNNING while
// trials dispatch, INTERRUPTED on cancel/error, COMPLETE on normal finish. The
// API reader may synthesize UNKNOWN when progress is absent (§9.2).
type Status string

const (
	StatusRunning     Status = "RUNNING"
	StatusInterrupted Status = "INTERRUPTED"
	StatusComplete    Status = "COMPLETE"
)

// TrialState is the per-trial terminal state (§7.4): COMPLETE (objectives
// present) or FAIL (empty metrics + an error message).
type TrialState string

const (
	TrialComplete TrialState = "COMPLETE"
	TrialFail     TrialState = "FAIL"
)

// WalkForward is the study.json walk_forward block (§7.2).
type WalkForward struct {
	Enabled     bool `json:"enabled"`
	Folds       int  `json:"folds"`
	EmbargoDays int  `json:"embargo_days"`
}

// StudyConfig is the study identity + configuration (study.json, §7.2). It is
// rewritten at every run_study start; CreatedAt is preserved on resume,
// UpdatedAt refreshed.
type StudyConfig struct {
	Version     int         `json:"version"`
	StudyName   string      `json:"study_name"`
	Strategy    string      `json:"strategy"`
	Start       string      `json:"start"`
	End         string      `json:"end"`
	Directions  []string    `json:"directions"`
	Objectives  []string    `json:"objectives"`
	Seed        int64       `json:"seed"`
	NTrials     int         `json:"n_trials"`
	Workers     int         `json:"workers"`
	WalkForward WalkForward `json:"walk_forward"`
	CreatedAt   time.Time   `json:"created_at"`
	UpdatedAt   time.Time   `json:"updated_at"`
	// TrialTimeoutSec is the per-trial timeout in whole seconds persisted to the
	// DB row (hyperopt_studies.trial_timeout_sec). nil disables the deadline
	// (§6.1 / §11). It is NOT part of the study.json artifact schema (§7.2), so
	// it carries json:"-" and the artifact writer never emits it.
	TrialTimeoutSec *int `json:"-"`
}

// CurrentBest is the progress.json current_best block (§6.8/§7.3): the argmax of
// sharpe+calmar over COMPLETE trials (first-seen wins ties).
type CurrentBest struct {
	Trial  int     `json:"trial"`
	Sharpe float64 `json:"sharpe"`
	Calmar float64 `json:"calmar"`
}

// Progress is the live study state (progress.json, §7.3). Nullable fields use
// pointers so they serialize as JSON null when absent.
type Progress struct {
	Status          Status       `json:"status"`
	CompletedTrials int          `json:"completed_trials"`
	FailedTrials    int          `json:"failed_trials"`
	RunningTrials   int          `json:"running_trials"`
	TotalTrials     int          `json:"total_trials"`
	Workers         int          `json:"workers"`
	StartedAt       *time.Time   `json:"started_at"`
	UpdatedAt       *time.Time   `json:"updated_at"`
	LastHeartbeatAt *time.Time   `json:"last_heartbeat_at"`
	CoordinatorPID  *int         `json:"coordinator_pid"`
	CurrentBest     *CurrentBest `json:"current_best"`
	LastError       *string      `json:"last_error"`
}

// FoldMetric is one fold's payload in trial_*.json folds[] (§4.3): the fold
// index first, then the fold's own BacktestMetrics.
type FoldMetric struct {
	Fold    int
	Metrics metrics.BacktestMetrics
}

// TrialArtifact is one trial's record (trial_%04d.json, §7.4). For joint studies
// Params is the nested per-sub-strategy map; otherwise it is the flat unprefixed
// map. Metrics is the empty struct when FAIL; Folds is empty when single-window
// or FAIL.
type TrialArtifact struct {
	Number       int
	OptunaNumber *int // sampler-side number; may drift from Number on resume (Q3)
	Strategy     string
	// Params holds the OPTUNA-recorded params (pre-constraint-clamp values, §2.3
	// Q5). For a single strategy it is {param: value}; for joint it is
	// {"sepa":{...}, "sector_rotation":{...}, "pairs":{...}}.
	Params     map[string]any
	Metrics    metrics.BacktestMetrics
	Folds      []FoldMetric
	State      TrialState
	StartedAt  time.Time
	FinishedAt *time.Time
	DurationS  float64
	RunDumpTS  *string
	Error      *string
}
