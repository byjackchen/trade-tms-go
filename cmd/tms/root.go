package main

import (
	"fmt"

	"github.com/rs/zerolog"
	"github.com/spf13/cobra"

	"github.com/byjackchen/trade-tms-go/internal/app"
	"github.com/byjackchen/trade-tms-go/internal/config"
)

// runtimeEnv bundles what every subcommand needs: the loaded config and a
// configured logger. Built once in PersistentPreRunE.
type runtimeEnv struct {
	cfg *config.Config
	log zerolog.Logger
}

func newRootCmd() *cobra.Command {
	env := &runtimeEnv{}

	root := &cobra.Command{
		Use:   "tms",
		Short: "Trade Management System — multi-strategy backtesting, hyperopt and live trading",
		Long: "tms is the Go port of the trade-multi-strategies system: one binary\n" +
			"covering data import, backtests, hyper-parameter optimization, the EOD\n" +
			"workflow, live trading and the HTTP/WebSocket API.",
		SilenceUsage: true, // runtime errors should not dump usage
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			logger, err := app.NewLogger(cfg.LogLevel, cfg.LogFormat)
			if err != nil {
				return err
			}
			env.cfg = cfg
			env.log = logger
			return nil
		},
	}

	root.AddCommand(
		newVersionCmd(),
		newMigrateCmd(env),
		newImportCmd(env),
		newStubCmd(env, "backtest", "Run a multi-strategy backtest over the historical universe"),
		newStubCmd(env, "hyperopt", "Run hyper-parameter optimization studies over the backtest engine"),
		newStubCmd(env, "live", "Run the live trading node (signal / paper / live modes)"),
		newStubCmd(env, "eod", "Run the end-of-day workflow (data sync, signals, watchlist, reports)"),
		newStubCmd(env, "api", "Serve the HTTP/WebSocket API for the UI"),
	)
	return root
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print build version information",
		Args:  cobra.NoArgs,
		// Own no-op hook: version must work even with a broken environment
		// (cobra runs the closest PersistentPreRunE, skipping the root's
		// config loading for this command only).
		PersistentPreRunE: func(*cobra.Command, []string) error { return nil },
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintln(cmd.OutOrStdout(), app.VersionString())
		},
	}
}

// notImplementedError is the sentinel failure for planned-but-unbuilt
// subcommands: exit non-zero with an explicit message instead of silently
// pretending to work.
func notImplementedError(name string) error {
	return fmt.Errorf("%q is not implemented yet — scheduled for a later build phase (the Python reference behaviour is the contract)", name)
}

func newStubCmd(env *runtimeEnv, name, short string) *cobra.Command {
	return &cobra.Command{
		Use:   name,
		Short: short + " (not implemented yet)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			env.log.Error().Str("command", name).Msg("subcommand not implemented yet")
			return notImplementedError(name)
		},
	}
}
