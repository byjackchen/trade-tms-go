package main

import (
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/byjackchen/trade-tms-go/internal/db"
)

// newMigrateCmd wires the embedded golang-migrate migrations. This is the
// only command with real behaviour in the P0 scaffold: the compose
// `migrate` service runs `tms migrate up` against TimescaleDB.
func newMigrateCmd(env *runtimeEnv) *cobra.Command {
	migrateCmd := &cobra.Command{
		Use:   "migrate",
		Short: "Manage the database schema (embedded migrations)",
	}

	up := &cobra.Command{
		Use:   "up",
		Short: "Apply all pending migrations",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			env.log.Info().
				Str("host", env.cfg.PGHost).
				Int("port", env.cfg.PGPort).
				Str("database", env.cfg.PGDatabase).
				Msg("applying migrations")
			if err := db.MigrateUp(env.cfg); err != nil {
				return err
			}
			version, dirty, err := db.MigrateStatus(env.cfg)
			if err != nil {
				return err
			}
			env.log.Info().Uint("version", version).Bool("dirty", dirty).Msg("migrations up to date")
			return nil
		},
	}

	down := &cobra.Command{
		Use:   "down [steps]",
		Short: "Roll back N migrations (default 1)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			steps := 1
			if len(args) == 1 {
				n, err := strconv.Atoi(args[0])
				if err != nil || n <= 0 {
					return fmt.Errorf("steps must be a positive integer, got %q", args[0])
				}
				steps = n
			}
			env.log.Warn().Int("steps", steps).Msg("rolling back migrations")
			if err := db.MigrateDown(env.cfg, steps); err != nil {
				return err
			}
			version, dirty, err := db.MigrateStatus(env.cfg)
			if err != nil {
				return err
			}
			env.log.Info().Uint("version", version).Bool("dirty", dirty).Msg("rollback complete")
			return nil
		},
	}

	status := &cobra.Command{
		Use:   "status",
		Short: "Show current schema version and dirty flag",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			version, dirty, err := db.MigrateStatus(env.cfg)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "version=%d dirty=%v\n", version, dirty)
			return nil
		},
	}

	migrateCmd.AddCommand(up, down, status)
	return migrateCmd
}
