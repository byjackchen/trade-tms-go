package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/redis/go-redis/v9"
	"github.com/spf13/cobra"

	"github.com/byjackchen/trade-tms-go/internal/db"
	"github.com/byjackchen/trade-tms-go/internal/jobs"
	"github.com/byjackchen/trade-tms-go/internal/jobs/handlers"
)

// newEODCmd implements `tms eod --as-of <date>`: the idempotent EOD
// engine-replay refresh (P5 locked decision 4). It replays
// [as_of-window, as_of] bars through the SAME engine as backtest, UPSERTs each
// strategy's evaluate_intent into tms.signal_intents idempotently on
// (strategy_id, symbol, as_of) — a re-run OVERWRITES, never duplicates — and
// publishes to Redis. With --enqueue it submits an eod.refresh job to the
// durable queue instead of running inline.
func newEODCmd(env *runtimeEnv) *cobra.Command {
	var (
		asOf         string
		strategy     string
		tickersCSV   string
		orbSymbol    string
		traderID     string
		startBalance float64
		windowDays   int
		enqueue      bool
		dedupeKey    string
	)

	cmd := &cobra.Command{
		Use:   "eod",
		Short: "Run the idempotent EOD engine-replay refresh for an as-of date",
		Long: "Refreshes the signal intents for an as-of date by replaying the\n" +
			"[as_of-window, as_of] bars through the SAME deterministic engine as\n" +
			"backtest, then UPSERTing each strategy's evaluate_intent into\n" +
			"tms.signal_intents idempotently on (strategy_id, symbol, as_of) and\n" +
			"publishing to Redis. Running twice for the same as_of produces the SAME\n" +
			"rows (no duplicates). With --enqueue, submits an eod.refresh job.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(asOf) == "" {
				return fmt.Errorf("--as-of is required (YYYY-MM-DD)")
			}
			var tickers []string
			for _, t := range strings.Split(tickersCSV, ",") {
				if t = strings.ToUpper(strings.TrimSpace(t)); t != "" {
					tickers = append(tickers, t)
				}
			}
			payload := map[string]any{
				"as_of":            asOf,
				"strategy":         strategyOrDefault(strategy),
				"starting_balance": startBalance,
			}
			if len(tickers) > 0 {
				payload["tickers"] = tickers
			}
			if s := strings.TrimSpace(orbSymbol); s != "" {
				payload["orb_symbol"] = strings.ToUpper(s)
			}
			if s := strings.TrimSpace(traderID); s != "" {
				payload["trader_id"] = s
			}
			if windowDays > 0 {
				payload["window_days"] = windowDays
			}
			b, err := json.Marshal(payload)
			if err != nil {
				return fmt.Errorf("marshaling eod payload: %w", err)
			}
			if enqueue {
				return enqueueEOD(cmd, env, b, dedupeKey)
			}
			return runEODInline(cmd.Context(), env, b)
		},
	}

	cmd.Flags().StringVar(&asOf, "as-of", "", "refresh date (YYYY-MM-DD, required)")
	cmd.Flags().StringVar(&strategy, "strategy", "multi", "strategy: sepa | sector_rotation | pairs | orb | multi")
	cmd.Flags().StringVar(&tickersCSV, "tickers", "", "comma-separated SEPA stock universe (sepa/multi)")
	cmd.Flags().StringVar(&orbSymbol, "orb-symbol", "", "ORB strategy: the single intraday instrument symbol")
	cmd.Flags().StringVar(&traderID, "trader-id", "", "Redis namespace for published updates (empty = PG only)")
	cmd.Flags().Float64Var(&startBalance, "starting-balance", 100000.0, "informational health-NAV starting balance")
	cmd.Flags().IntVar(&windowDays, "window-days", 0, "replay window calendar days (default 400)")
	cmd.Flags().BoolVar(&enqueue, "enqueue", false, "enqueue an eod.refresh job instead of running inline")
	cmd.Flags().StringVar(&dedupeKey, "dedupe-key", "", "dedupe key for --enqueue (at most one active job per key)")
	return cmd
}

func strategyOrDefault(s string) string {
	if s = strings.TrimSpace(s); s != "" {
		return s
	}
	return "multi"
}

// runEODInline runs the EOD refresh in-process (no queue), printing the report.
func runEODInline(ctx context.Context, env *runtimeEnv, payload json.RawMessage) error {
	pool, err := db.NewPool(ctx, env.cfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	// Redis publisher is best-effort: without it the refresh still upserts to PG.
	redisClient := redis.NewClient(&redis.Options{
		Addr:     env.cfg.RedisAddr,
		DB:       env.cfg.RedisDB,
		Password: env.cfg.RedisPassword,
	})
	defer func() { _ = redisClient.Close() }()
	if perr := redisClient.Ping(ctx).Err(); perr != nil {
		env.log.Warn().Err(perr).Msg("redis unreachable; EOD refresh publishes nothing (PG upsert unaffected)")
		_ = redisClient.Close()
		redisClient = nil
	}

	h, err := handlers.NewEODRefresh(pool, redisClient, env.cfg.StrategyParamsDir, env.log)
	if err != nil {
		return err
	}
	job := &jobs.Job{ID: 0, Kind: handlers.KindEODRefresh, Payload: payload}
	report := func(_ context.Context, p any) error {
		env.log.Debug().Interface("progress", p).Msg("eod progress")
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

// enqueueEOD submits an eod.refresh job to the durable queue.
func enqueueEOD(cmd *cobra.Command, env *runtimeEnv, payload json.RawMessage, dedupeKey string) error {
	return withQueue(cmd, env, func(q *jobs.Queue) error {
		job, deduped, err := q.Enqueue(cmd.Context(), jobs.EnqueueParams{
			Kind:      handlers.KindEODRefresh,
			Payload:   payload,
			DedupeKey: dedupeKey,
			Actor:     "cli",
		})
		if err != nil {
			return err
		}
		if deduped {
			fmt.Fprintf(cmd.OutOrStdout(), "deduped: active eod job %d (status=%s) already holds key %q\n",
				job.ID, job.Status, dedupeKey)
			return nil
		}
		fmt.Fprintf(cmd.OutOrStdout(), "enqueued eod.refresh job %d\n", job.ID)
		return nil
	})
}
