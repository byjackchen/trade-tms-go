package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/rs/zerolog"
	"github.com/spf13/cobra"

	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/data/sharadar"
	"github.com/byjackchen/trade-tms-go/internal/db"
)

// newSyncCmd declares `tms sync`: the live Nasdaq Data Link sync engine's
// CLI entry point (the relational counterpart of the Python
// `make sync-universe`, spec docs/spec/data-sharadar.md §8/§9). It is the
// operator-facing twin of the worker's data.refresh source=api handler:
// both drive internal/data/sharadar.Syncer.
//
//   - `tms sync bootstrap --start --end [--ticker ...]` backfills all five
//     datasets over a bounded window (TICKERS->SEP->SFP->SF1->EVENTS); a
//     failed step aborts (CLI parity, spec §9).
//   - `tms sync catchup` runs the watermark-driven incremental catchup
//     through T-1 (EnsureFresh / ensure_cache_fresh, spec §8); warn-and-
//     continue, never auto-bootstraps.
//
// Every run records tms.dataset_sync_runs audit rows and advances the
// tms.dataset_sync watermark. All "today"/trading-date logic is normalized
// to the America/New_York trading date (P1 locked decision 2).
func newSyncCmd(env *runtimeEnv) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Live Nasdaq Data Link sync (bootstrap | catchup) into TimescaleDB",
		Long: "Drives the Nasdaq Data Link -> PostgreSQL sync engine\n" +
			"(internal/data/sharadar.Syncer): a bounded `bootstrap` backfill or the\n" +
			"watermark-driven `catchup` (ensure-fresh). Requires\n" +
			"TMS_NASDAQ_DATA_LINK_API_KEY. The worker's data.refresh source=api job\n" +
			"runs the same catchup engine.",
	}
	cmd.AddCommand(
		newSyncBootstrapCmd(env),
		newSyncCatchupCmd(env),
	)
	return cmd
}

// withSyncer builds a live Syncer (client + NYSE calendar + pgStore) for one
// CLI invocation and hands it to fn. The API key is required here (fail loud
// before any work, spec §1 [MUST-MATCH]).
func withSyncer(cmd *cobra.Command, env *runtimeEnv, fn func(ctx context.Context, s *sharadar.Syncer) error) error {
	ctx := cmd.Context()

	key, err := env.cfg.Require("TMS_NASDAQ_DATA_LINK_API_KEY",
		"get a key at https://data.nasdaq.com/account/profile")
	if err != nil {
		return err
	}

	log := env.log.With().Str("cmd", "sync").Logger()
	client, err := sharadar.NewClient(key, sharadar.WithLogger(log))
	if err != nil {
		return err
	}
	cal, err := calendar.NewNYSE()
	if err != nil {
		return err
	}

	pool, err := db.NewPool(ctx, env.cfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	syncer, err := sharadar.NewSyncer(pool, client, cal, sharadar.WithSyncLogger(log))
	if err != nil {
		return err
	}
	return fn(ctx, syncer)
}

func newSyncBootstrapCmd(env *runtimeEnv) *cobra.Command {
	var (
		startStr string
		endStr   string
		tickers  []string
	)
	cmd := &cobra.Command{
		Use:   "bootstrap",
		Short: "Backfill all five datasets over a bounded date window",
		Long: "Backfills TICKERS -> SEP -> SFP -> SF1 -> EVENTS over [--start, --end]\n" +
			"(inclusive). SEP/SFP are pulled in calendar-quarter chunks; SF1/EVENTS\n" +
			"in 500-ticker batches over full history. A failed step aborts (the\n" +
			"merge is idempotent, so re-running converges). With --ticker the SEP/SFP\n" +
			"backfill is narrowed to those symbols (TICKERS is always synced in full\n" +
			"— the survivorship policy, not the selection).",
		Example: "  tms sync bootstrap --start 2020-01-01 --end 2026-06-12\n" +
			"  tms sync bootstrap --start 2026-06-06 --end 2026-06-12 --ticker AAPL --ticker KO",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			start, err := calendar.ParseDate(startStr)
			if err != nil {
				return fmt.Errorf("invalid --start %q (want YYYY-MM-DD): %w", startStr, err)
			}
			end, err := calendar.ParseDate(endStr)
			if err != nil {
				return fmt.Errorf("invalid --end %q (want YYYY-MM-DD): %w", endStr, err)
			}
			if end.Before(start) {
				return fmt.Errorf("--end %s is before --start %s", end, start)
			}
			return withSyncer(cmd, env, func(ctx context.Context, s *sharadar.Syncer) error {
				rows, runErr := s.Bootstrap(ctx, sharadar.BootstrapOptions{
					Start:   start,
					End:     end,
					Tickers: tickers,
				})
				// Bootstrap returns partial counts even on error; print what
				// landed so the operator can see progress before the abort.
				printSyncResult(cmd, map[string]any{
					"flow":       "bootstrap",
					"start":      start.String(),
					"end":        end.String(),
					"rows_added": rows,
				})
				return runErr
			})
		},
	}
	cmd.Flags().StringVar(&startStr, "start", "", "backfill window start (YYYY-MM-DD, inclusive) — required")
	cmd.Flags().StringVar(&endStr, "end", "", "backfill window end (YYYY-MM-DD, inclusive) — required")
	cmd.Flags().StringArrayVar(&tickers, "ticker", nil, "restrict SEP/SFP/SF1/EVENTS to this symbol (repeatable; default: full universe)")
	_ = cmd.MarkFlagRequired("start")
	_ = cmd.MarkFlagRequired("end")
	return cmd
}

func newSyncCatchupCmd(env *runtimeEnv) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "catchup",
		Short: "Incrementally catch the store up to T-1 (watermark-driven)",
		Long: "Runs the ensure-fresh catchup: per-trading-day SEP+SFP updates from the\n" +
			"SEP watermark through T-1, then a TICKERS overwrite and an incremental\n" +
			"SF1/EVENTS refresh. Never auto-bootstraps (an un-bootstrapped store is\n" +
			"reported as skipped — run `tms sync bootstrap` first). Per-step failures\n" +
			"are warn-and-continue; the watermark is persisted after every step.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withSyncer(cmd, env, func(ctx context.Context, s *sharadar.Syncer) error {
				rep, runErr := s.EnsureFresh(ctx)
				printSyncResult(cmd, catchupCLIResult(rep))
				return runErr
			})
		},
	}
	return cmd
}

// catchupCLIResult renders a CatchupReport for CLI output.
func catchupCLIResult(rep *sharadar.CatchupReport) map[string]any {
	out := map[string]any{"flow": "catchup"}
	if rep == nil {
		return out
	}
	out["did_work"] = rep.DidWork()
	out["days_attempted"] = rep.DaysAttempted
	out["days_succeeded"] = rep.DaysSucceeded
	if rep.SkippedReason != "" {
		out["skipped_reason"] = rep.SkippedReason
	}
	if rep.RowsAdded != nil {
		out["rows_added"] = rep.RowsAdded
	}
	if len(rep.Errors) > 0 {
		out["errors"] = rep.Errors
	}
	return out
}

// printSyncResult writes the result object as indented JSON to stdout.
func printSyncResult(cmd *cobra.Command, result map[string]any) {
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	if err := enc.Encode(result); err != nil {
		// stdout encoding failure is non-fatal; surface it on the logger.
		zerolog.Ctx(cmd.Context()).Warn().Err(err).Msg("encoding sync result failed")
	}
}
