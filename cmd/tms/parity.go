package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/byjackchen/trade-tms-go/internal/parity"
)

// newParityBacktestCmd implements `tms parity-backtest`: run the canonical
// deterministic parity SCRIPT through the Go engine ZERO-COST (nautilus-compat
// fill profile) over the SAME wrangled bars the reference Nautilus harness
// consumed, and dump the legacy runs/<ts>/*.json artifact set for field-by-field
// comparison. This is the Go half of the golden-parity GATE.
func newParityBacktestCmd(env *runtimeEnv) *cobra.Command {
	var (
		scriptPath string
		barsPath   string
		runsRoot   string
		runTS      string
	)

	cmd := &cobra.Command{
		Use:   "parity-backtest",
		Short: "Run the golden-parity backtest (scripted engine, nautilus-compat) and dump runs/<ts> artifacts",
		Long: "parity-backtest drives the Go engine over a canonical order script and the\n" +
			"wrangled bars the reference Nautilus harness produced, then emits the\n" +
			"runs/<ts>/*.json artifacts the comparator diffs against the Nautilus dump.\n" +
			"It runs the zero-cost nautilus-compat fill profile (the accuracy gate).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var ts time.Time
			if runTS != "" {
				t, err := time.Parse("2006-01-02_15-04-05", runTS)
				if err != nil {
					return fmt.Errorf("parity-backtest: bad --run-ts %q (want 2006-01-02_15-04-05): %w", runTS, err)
				}
				ts = t.UTC()
			}
			res, err := parity.Run(cmd.Context(), parity.RunOptions{
				ScriptPath: scriptPath,
				BarsPath:   barsPath,
				RunsRoot:   runsRoot,
				Timestamp:  ts,
			})
			if err != nil {
				return err
			}
			env.log.Info().
				Str("run_dir", res.RunDir).
				Float64("final_balance_usd", res.FinalBalance).
				Float64("total_pnl_usd", res.TotalPnL).
				Int("fills", res.NumFills).
				Int("bars", res.BarsProcessed).
				Int("sampled_days", res.SampledDays).
				Msg("parity backtest complete")
			fmt.Fprintln(cmd.OutOrStdout(), res.RunDir)
			return nil
		},
	}

	cmd.Flags().StringVar(&scriptPath, "script", "testdata/parity/script_canonical.json", "canonical order script JSON")
	cmd.Flags().StringVar(&barsPath, "bars", "tmp/parity/nautilus_out/bars.json", "shared wrangled bars JSON (from the Nautilus harness)")
	cmd.Flags().StringVar(&runsRoot, "runs-root", "tmp/parity/go_out", "runs/ root the artifacts are dumped under")
	cmd.Flags().StringVar(&runTS, "run-ts", "", "pin the run directory name (2006-01-02_15-04-05 UTC); defaults to now")
	return cmd
}
