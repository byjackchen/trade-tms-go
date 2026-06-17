package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/byjackchen/trade-tms-go/internal/data/sharadar"
	"github.com/byjackchen/trade-tms-go/internal/db"
)

// newImportCmd declares the data-import command tree. The concrete importer is
// the `sharadar` subcommand (bulk-loads the Sharadar parquet cache into
// TimescaleDB); the parent groups it and prints help when invoked bare.
func newImportCmd(env *runtimeEnv) *cobra.Command {
	importCmd := &cobra.Command{
		Use:   "import",
		Short: "Import market data into TimescaleDB (Sharadar datasets)",
		Long: "Import Sharadar datasets into TimescaleDB from an existing local cache\n" +
			"(cache/sharadar parquet layout). See `tms import sharadar`.",
		Args:          cobra.NoArgs,
		SilenceErrors: false,
		// No bare action: running `tms import` with no subcommand prints help
		// (cobra default when RunE is nil and subcommands exist).
	}

	importCmd.AddCommand(newImportSharadarCmd(env))
	return importCmd
}

// newImportSharadarCmd implements `tms import sharadar`: bulk-load the
// supported cache/sharadar parquet layout (TICKERS, SEP, SFP, SF1,
// EVENTS) into TimescaleDB with idempotent upsert semantics. See
// internal/data/sharadar for the conversion and merge contracts.
func newImportSharadarCmd(env *runtimeEnv) *cobra.Command {
	var (
		cacheDir      string
		tablesCSV     string
		tickersCSV    string
		sinceStr      string
		batchSize     int
		progressEvery int64
		failOnErrors  bool
	)

	cmd := &cobra.Command{
		Use:   "sharadar",
		Short: "Import the Sharadar parquet cache (TICKERS/SEP/SFP/SF1/EVENTS) into TimescaleDB",
		Long: "Reads the supported cache/sharadar parquet layout and\n" +
			"bulk-loads it into TimescaleDB (pgx CopyFrom into a staging temp table,\n" +
			"then INSERT ... ON CONFLICT merges — idempotent, 'new rows win'\n" +
			"semantics). Float prices become int64 1e-4 fixed point via\n" +
			"the Decimal(str(x)) half-even bridge; NaN maps to NULL.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()

			root, err := sharadar.ResolveCacheDir(cacheDir, env.cfg.SharadarCacheDir)
			if err != nil {
				return err
			}

			var since time.Time
			if sinceStr != "" {
				since, err = time.ParseInLocation("2006-01-02", sinceStr, time.UTC)
				if err != nil {
					return fmt.Errorf("invalid --since %q (want YYYY-MM-DD): %w", sinceStr, err)
				}
			}

			opts := sharadar.Options{
				CacheDir:      root,
				Tables:        splitCSV(tablesCSV),
				Tickers:       splitCSV(tickersCSV),
				Since:         since,
				BatchSize:     batchSize,
				ProgressEvery: progressEvery,
			}

			pool, err := db.NewPool(ctx, env.cfg)
			if err != nil {
				return err
			}
			defer pool.Close()

			imp, err := sharadar.New(pool, env.log, opts)
			if err != nil {
				return err
			}
			summary, runErr := imp.Run(ctx)
			if summary != nil {
				fmt.Fprint(cmd.OutOrStdout(), summary.String())
			}
			if runErr != nil {
				return runErr
			}
			if failOnErrors && summary.Failed() {
				return errors.New("import completed with captured errors (see summary); rerun is safe — upserts are idempotent")
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&cacheDir, "cache-dir", "", "Sharadar cache root (default: TMS_SHARADAR_CACHE_DIR, then <repo root>/cache/sharadar)")
	cmd.Flags().StringVar(&tablesCSV, "tables", "all", "comma-separated datasets: tickers,sep,sfp,sf1,events or all")
	cmd.Flags().StringVar(&tickersCSV, "tickers", "", "comma-separated ticker filter (default: all tickers)")
	cmd.Flags().StringVar(&sinceStr, "since", "", "drop rows dated before YYYY-MM-DD (SEP/SFP/EVENTS: date, SF1: datekey)")
	cmd.Flags().IntVar(&batchSize, "batch-size", sharadar.DefaultBatchSize, "rows staged per CopyFrom+merge flush (memory bound)")
	cmd.Flags().Int64Var(&progressEvery, "progress-every", sharadar.DefaultProgressEvery, "log progress every N source rows")
	cmd.Flags().BoolVar(&failOnErrors, "fail-on-errors", true, "exit non-zero when any per-file/per-row error was captured")
	return cmd
}

// splitCSV splits a comma-separated flag value into trimmed non-empty parts.
func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
