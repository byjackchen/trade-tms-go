package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/byjackchen/trade-tms-go/internal/db"
	"github.com/byjackchen/trade-tms-go/internal/jobs"
	"github.com/byjackchen/trade-tms-go/internal/jobs/handlers"
)

// newBacktestCmd implements `tms backtest`: run a deterministic backtest
// through internal/engine over the TimescaleDB bars, persisting the result to
// research.* (DB source of truth) and emitting the legacy runs/{ts}/*.json
// artifact set. Flags mirror the backtest.run job params; --enqueue submits the
// job to the durable queue instead of running inline (for the worker / gate).
func newBacktestCmd(env *runtimeEnv) *cobra.Command {
	var (
		tickersCSV   string
		start        string
		end          string
		startBalance float64
		fillProfile  string
		intentsPath  string
		intentsJSON  string
		kind         string
		runTS        string
		seed         int64
		enqueue      bool
		dedupeKey    string
		maxAttempts  int32
	)

	cmd := &cobra.Command{
		Use:   "backtest",
		Short: "Run a deterministic backtest (scripted engine) and persist results",
		Long: "Runs a backtest through the Go engine over the historical universe and\n" +
			"persists the result to the database (research.runs/run_metrics/\n" +
			"equity_curves/trades) as the source of truth, also emitting the legacy\n" +
			"runs/{ts}/*.json artifact set (TMS_RUNS_DIR). With --enqueue the run is\n" +
			"submitted to the durable job queue (backtest.run) for the worker.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			payload, err := buildBacktestPayload(backtestPayloadArgs{
				tickersCSV:   tickersCSV,
				start:        start,
				end:          end,
				startBalance: startBalance,
				fillProfile:  fillProfile,
				intentsPath:  intentsPath,
				intentsJSON:  intentsJSON,
				kind:         kind,
				runTS:        runTS,
				seed:         seed,
			})
			if err != nil {
				return err
			}
			if enqueue {
				return enqueueBacktest(cmd, env, payload, dedupeKey, maxAttempts)
			}
			return runBacktestInline(cmd.Context(), env, payload)
		},
	}

	cmd.Flags().StringVar(&tickersCSV, "tickers", "", "comma-separated ticker list (e.g. AAPL,KO,MSFT)")
	cmd.Flags().StringVar(&start, "start", "", "bar window start (YYYY-MM-DD, required)")
	cmd.Flags().StringVar(&end, "end", "", "bar window end (YYYY-MM-DD, required)")
	cmd.Flags().Float64Var(&startBalance, "starting-balance", 100000.0, "starting account balance in USD")
	cmd.Flags().StringVar(&fillProfile, "fill-profile", "nautilus-compat", "fill model: nautilus-compat | realistic")
	cmd.Flags().StringVar(&intentsPath, "intents", "", "path to a JSON array of scripted intents")
	cmd.Flags().StringVar(&intentsJSON, "intents-json", "", "inline JSON array of scripted intents (overrides --intents)")
	cmd.Flags().StringVar(&kind, "kind", "multi-strategy", "run kind badge")
	cmd.Flags().StringVar(&runTS, "run-ts", "", "pin the run directory name / idempotency key (UTC %Y-%m-%d_%H-%M-%S)")
	cmd.Flags().Int64Var(&seed, "seed", 0, "RNG seed (reserved for stochastic fill models)")
	cmd.Flags().BoolVar(&enqueue, "enqueue", false, "enqueue a backtest.run job instead of running inline")
	cmd.Flags().StringVar(&dedupeKey, "dedupe-key", "", "dedupe key for --enqueue (at most one active job per key)")
	cmd.Flags().Int32Var(&maxAttempts, "max-attempts", 1, "max attempts for --enqueue")
	return cmd
}

type backtestPayloadArgs struct {
	tickersCSV   string
	start        string
	end          string
	startBalance float64
	fillProfile  string
	intentsPath  string
	intentsJSON  string
	kind         string
	runTS        string
	seed         int64
}

// buildBacktestPayload assembles the backtest.run payload from CLI flags,
// validating the required ones up front.
func buildBacktestPayload(a backtestPayloadArgs) (json.RawMessage, error) {
	if strings.TrimSpace(a.start) == "" || strings.TrimSpace(a.end) == "" {
		return nil, fmt.Errorf("--start and --end are required (YYYY-MM-DD)")
	}
	var tickers []string
	for _, t := range strings.Split(a.tickersCSV, ",") {
		t = strings.ToUpper(strings.TrimSpace(t))
		if t != "" {
			tickers = append(tickers, t)
		}
	}
	if len(tickers) == 0 {
		return nil, fmt.Errorf("--tickers is required (comma-separated)")
	}

	var intents []any
	raw := strings.TrimSpace(a.intentsJSON)
	if raw == "" && a.intentsPath != "" {
		b, err := os.ReadFile(a.intentsPath)
		if err != nil {
			return nil, fmt.Errorf("reading --intents %q: %w", a.intentsPath, err)
		}
		raw = strings.TrimSpace(string(b))
	}
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &intents); err != nil {
			return nil, fmt.Errorf("invalid intents JSON (want an array): %w", err)
		}
	}

	payload := map[string]any{
		"tickers":          tickers,
		"start":            a.start,
		"end":              a.end,
		"starting_balance": a.startBalance,
		"fill_profile":     a.fillProfile,
		"strategy":         "scripted",
		"kind":             a.kind,
		"seed":             a.seed,
	}
	if intents != nil {
		payload["intents"] = intents
	}
	if a.runTS != "" {
		payload["run_ts"] = a.runTS
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshaling payload: %w", err)
	}
	return b, nil
}

// enqueueBacktest submits a backtest.run job to the durable queue.
func enqueueBacktest(cmd *cobra.Command, env *runtimeEnv, payload json.RawMessage, dedupeKey string, maxAttempts int32) error {
	return withQueue(cmd, env, func(q *jobs.Queue) error {
		job, deduped, err := q.Enqueue(cmd.Context(), jobs.EnqueueParams{
			Kind:        handlers.KindBacktestRun,
			Payload:     payload,
			DedupeKey:   dedupeKey,
			MaxAttempts: maxAttempts,
			Actor:       "cli",
		})
		if err != nil {
			return err
		}
		if deduped {
			fmt.Fprintf(cmd.OutOrStdout(), "deduped: active backtest job %d (status=%s) already holds key %q\n",
				job.ID, job.Status, dedupeKey)
			return nil
		}
		fmt.Fprintf(cmd.OutOrStdout(), "enqueued backtest.run job %d\n", job.ID)
		return nil
	})
}

// runBacktestInline runs the backtest in-process (no queue), printing the
// result JSON. Used for the CLI and the gate.
func runBacktestInline(ctx context.Context, env *runtimeEnv, payload json.RawMessage) error {
	pool, err := db.NewPool(ctx, env.cfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	h, err := handlers.NewBacktest(pool, env.cfg.RunsDir, env.log)
	if err != nil {
		return err
	}
	// A synthetic in-memory job carries the payload; inline progress is logged.
	job := &jobs.Job{ID: 0, Kind: handlers.KindBacktestRun, Payload: payload}
	report := func(_ context.Context, p any) error {
		env.log.Debug().Interface("progress", p).Msg("backtest progress")
		return nil
	}
	result, err := h.Run(ctx, job, report)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(result)
}
