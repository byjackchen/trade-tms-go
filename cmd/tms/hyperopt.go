package main

// hyperopt.go implements `tms hyperopt`: run / list / promote hyper-parameter
// optimization studies over the backtest engine (internal/hyperopt/study). It
// mirrors the Python scripts/run_hyperopt.py flags (spec §11) on the Go command,
// plus list/promote subcommands for the control plane. `run` executes the study
// inline (in-process) by default or enqueues a hyperopt.run job with --enqueue.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/byjackchen/trade-tms-go/internal/db"
	"github.com/byjackchen/trade-tms-go/internal/hyperopt/study"
	"github.com/byjackchen/trade-tms-go/internal/jobs"
	"github.com/byjackchen/trade-tms-go/internal/jobs/handlers"
)

// newHyperoptCmd builds `tms hyperopt` with run/list/promote subcommands.
func newHyperoptCmd(env *runtimeEnv) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hyperopt",
		Short: "Run hyper-parameter optimization studies over the backtest engine",
		Long: "Runs a self-written NSGA-II walk-forward hyper-parameter study over the\n" +
			"deterministic Go backtest engine: each candidate's (sharpe, calmar)\n" +
			"objective is the aggregate of per-fold metrics over a shared read-only\n" +
			"bar dataset. Trials persist to research.hyperopt_* and a byte-compatible\n" +
			"runs/hyperopt/<ts>/ artifact tree.",
		Args: cobra.NoArgs,
	}
	cmd.AddCommand(newHyperoptRunCmd(env), newHyperoptListCmd(env), newHyperoptPromoteCmd(env))
	return cmd
}

func newHyperoptRunCmd(env *runtimeEnv) *cobra.Command {
	var (
		strategy     string
		start        string
		end          string
		population   int
		generations  int
		seed         int64
		workers      int
		walkForward  bool
		folds        int
		embargoDays  int
		tickersCSV   string
		startBalance float64
		studyTS      string
		trialTimeout int
		resume       bool
		enqueue      bool
		dedupeKey    string
		maxAttempts  int32
	)
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run (or enqueue) a hyper-parameter optimization study",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			switch strategy {
			case "sepa", "sector_rotation", "pairs", "joint":
			default:
				return fmt.Errorf("--strategy must be sepa|sector_rotation|pairs|joint")
			}
			if strings.TrimSpace(start) == "" || strings.TrimSpace(end) == "" {
				return fmt.Errorf("--start and --end are required (YYYY-MM-DD)")
			}
			var tickers []string
			for _, t := range strings.Split(tickersCSV, ",") {
				if t = strings.ToUpper(strings.TrimSpace(t)); t != "" {
					tickers = append(tickers, t)
				}
			}
			if (strategy == "sepa" || strategy == "joint") && len(tickers) == 0 {
				return fmt.Errorf("--strategy=%s requires --tickers (the stock universe)", strategy)
			}
			if resume && studyTS == "" {
				return fmt.Errorf("--resume requires --study-ts (the study to resume)")
			}
			payload := map[string]any{
				"strategy": strategy, "start": start, "end": end,
				"population": population, "generations": generations, "seed": seed,
				"workers": workers, "walk_forward": walkForward, "folds": folds,
				"embargo_days": embargoDays, "starting_balance": startBalance,
				// 0 disables the per-trial deadline (§11); >0 sets it in seconds.
				"trial_timeout_sec": trialTimeout,
			}
			if len(tickers) > 0 {
				payload["tickers"] = tickers
			}
			if studyTS != "" {
				payload["study_ts"] = studyTS
			}
			if resume {
				payload["resume"] = true
			}
			body, err := json.Marshal(payload)
			if err != nil {
				return err
			}
			if enqueue {
				return enqueueHyperopt(cmd, env, body, dedupeKey, maxAttempts)
			}
			return runHyperoptInline(cmd.Context(), env, body)
		},
	}
	cmd.Flags().StringVar(&strategy, "strategy", "", "study strategy: sepa|sector_rotation|pairs|joint (required)")
	cmd.Flags().StringVar(&start, "start", "2023-01-01", "study window start (YYYY-MM-DD)")
	cmd.Flags().StringVar(&end, "end", "2024-12-31", "study window end (YYYY-MM-DD)")
	cmd.Flags().IntVar(&population, "population", 50, "NSGA-II generation size")
	cmd.Flags().IntVar(&generations, "generations", 5, "number of generations")
	cmd.Flags().Int64Var(&seed, "seed", 42, "PRNG seed (deterministic study)")
	cmd.Flags().IntVar(&workers, "workers", 0, "evaluation parallelism (0 = min(cores-2,16))")
	cmd.Flags().BoolVar(&walkForward, "walk-forward", true, "walk-forward folds (vs single full-window)")
	cmd.Flags().IntVar(&folds, "folds", 5, "walk-forward folds")
	cmd.Flags().IntVar(&embargoDays, "embargo-days", 5, "walk-forward embargo days")
	cmd.Flags().StringVar(&tickersCSV, "tickers", "", "comma-separated SEPA/joint stock universe")
	cmd.Flags().Float64Var(&startBalance, "starting-balance", 100000.0, "starting account balance USD")
	cmd.Flags().StringVar(&studyTS, "study-ts", "", "pin the study dir / idempotency key (UTC %Y-%m-%d_%H-%M-%S)")
	cmd.Flags().IntVar(&trialTimeout, "trial-timeout-sec", 600, "per-trial timeout in seconds (0 disables)")
	cmd.Flags().BoolVar(&resume, "resume", false, "resume the study named by --study-ts (skips COMPLETE trials)")
	cmd.Flags().BoolVar(&enqueue, "enqueue", false, "enqueue a hyperopt.run job instead of running inline")
	cmd.Flags().StringVar(&dedupeKey, "dedupe-key", "", "dedupe key for --enqueue")
	cmd.Flags().Int32Var(&maxAttempts, "max-attempts", 1, "max attempts for --enqueue")
	return cmd
}

func enqueueHyperopt(cmd *cobra.Command, env *runtimeEnv, payload json.RawMessage, dedupeKey string, maxAttempts int32) error {
	return withQueue(cmd, env, func(q *jobs.Queue) error {
		job, deduped, err := q.Enqueue(cmd.Context(), jobs.EnqueueParams{
			Kind: handlers.KindHyperoptRun, Payload: payload,
			DedupeKey: dedupeKey, MaxAttempts: maxAttempts, Actor: "cli",
		})
		if err != nil {
			return err
		}
		if deduped {
			fmt.Fprintf(cmd.OutOrStdout(), "deduped: active hyperopt job %d (status=%s) already holds key %q\n",
				job.ID, job.Status, dedupeKey)
			return nil
		}
		fmt.Fprintf(cmd.OutOrStdout(), "enqueued hyperopt.run job %d\n", job.ID)
		return nil
	})
}

// runHyperoptInline runs the study in-process (no queue), printing the result.
func runHyperoptInline(ctx context.Context, env *runtimeEnv, payload json.RawMessage) error {
	pool, err := db.NewPool(ctx, env.cfg)
	if err != nil {
		return err
	}
	defer pool.Close()
	h, err := handlers.NewHyperopt(pool, env.cfg.RunsDir, env.cfg.StrategyParamsDir, env.log)
	if err != nil {
		return err
	}
	job := &jobs.Job{ID: 0, Kind: handlers.KindHyperoptRun, Payload: payload}
	report := func(_ context.Context, p any) error {
		env.log.Debug().Interface("progress", p).Msg("hyperopt progress")
		return nil
	}
	result, err := h.Run(ctx, job, report)
	if err != nil {
		return err
	}
	if m, ok := result.(map[string]any); ok {
		fmt.Printf("Study complete: %v\nArtifacts: %v\n", m["study_name"], m["study_dir"])
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(result)
}

func newHyperoptListCmd(env *runtimeEnv) *cobra.Command {
	var (
		strategy string
		limit    int
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List hyperopt studies (newest-first)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			pool, err := db.NewPool(cmd.Context(), env.cfg)
			if err != nil {
				return err
			}
			defer pool.Close()
			store := study.NewStore(pool)
			rows, err := store.List(cmd.Context(), strings.TrimSpace(strategy), limit)
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "STUDY_TS\tSTRATEGY\tSTATUS\tDONE/TOTAL\tFAILED\tBEST(SH,CA)")
			for _, r := range rows {
				best := "-"
				if r.CurrentBest != nil {
					best = fmt.Sprintf("t%d(%.2f,%.2f)", r.CurrentBest.Trial, r.CurrentBest.Sharpe, r.CurrentBest.Calmar)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%d/%d\t%d\t%s\n",
					r.TS, r.Strategy, r.Status, r.CompletedTrials, r.NTrials, r.FailedTrials, best)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().StringVar(&strategy, "strategy", "", "filter by strategy (sepa|sector_rotation|pairs|joint)")
	cmd.Flags().IntVar(&limit, "limit", 50, "max studies to list")
	return cmd
}

func newHyperoptPromoteCmd(env *runtimeEnv) *cobra.Command {
	var (
		studyTS string
		trialID int
		by      string
	)
	cmd := &cobra.Command{
		Use:   "promote",
		Short: "Promote a study trial's params to active_params (with audit)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(studyTS) == "" {
				return fmt.Errorf("--study is required (the study_ts)")
			}
			if trialID < 0 {
				return fmt.Errorf("--trial must be a non-negative integer")
			}
			pool, err := db.NewPool(cmd.Context(), env.cfg)
			if err != nil {
				return err
			}
			defer pool.Close()
			promoter := study.NewPromoter(pool)
			promotedBy := by
			if promotedBy == "" {
				promotedBy = "cli"
			}
			promoted, err := promoter.Promote(cmd.Context(), study.PromoteInput{
				StudyTS: studyTS, TrialNumber: trialID, PromotedBy: promotedBy, Now: time.Now(),
			})
			if err != nil {
				return err
			}
			for _, p := range promoted {
				fmt.Fprintf(cmd.OutOrStdout(), "promoted %s -> param_set %d (v%d)\n", p.Strategy, p.ParamSetID, p.Version)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&studyTS, "study", "", "study_ts to promote from (required)")
	cmd.Flags().IntVar(&trialID, "trial", 0, "artifact trial number to promote")
	cmd.Flags().StringVar(&by, "by", "cli", "promotion audit identity (promoted_by)")
	return cmd
}
