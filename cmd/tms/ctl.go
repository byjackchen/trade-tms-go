package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/redis/go-redis/v9"
	"github.com/spf13/cobra"

	"github.com/byjackchen/trade-tms-go/internal/app"
	"github.com/byjackchen/trade-tms-go/internal/commands"
	"github.com/byjackchen/trade-tms-go/internal/db"
	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// newCtlCmd implements `tms ctl <command>`: the operator control plane for the
// live trading node. Each subcommand ENQUEUES an audited ops.commands row (the
// HTTP API + this CLI are the only producers; the tms-live consumer is the only
// executor — the trading mutation surface stays out of process). Destructive
// commands (set_mode paper/live, flatten, emergency-kill) require --confirm.
//
//	tms ctl reconcile               — on-demand broker vs strategy-book reconcile
//	tms ctl flatten --confirm       — close ALL positions (kill-switch)
//	tms ctl emergency-kill --confirm — halt + flatten + stop (panic button)
//	tms ctl halt | resume | stop | kill
//	tms ctl set-mode --exec-policy signal|auto [--env simu|paper|real] [--confirm]
func newCtlCmd(env *runtimeEnv) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ctl",
		Short: "Operator control plane: enqueue audited live-node commands",
		Long: "Enqueues audited ops.commands rows the live trading node executes\n" +
			"(reconcile / flatten / emergency-kill / halt / resume / stop / kill /\n" +
			"set-mode). Destructive commands require --confirm.",
		Args: cobra.NoArgs,
	}
	cmd.AddCommand(
		ctlSimple(env, "reconcile", commands.NameReconcile, "Run an on-demand broker vs strategy-book reconciliation", false),
		ctlSimple(env, "flatten", commands.NameFlatten, "Close ALL open positions (kill-switch; --confirm required)", true),
		ctlSimple(env, "emergency-kill", commands.NameEmergencyKill, "Panic button: halt + flatten + stop (--confirm required)", true),
		ctlSimple(env, "halt", commands.NameHalt, "Halt: stop emitting new intents / suppress new opens", false),
		ctlSimple(env, "resume", commands.NameResume, "Resume after a halt", false),
		ctlSimple(env, "stop", commands.NameStop, "Stop the node gracefully", false),
		ctlSimple(env, "kill", commands.NameKill, "Kill switch: halt + stop", false),
		newSetModeCmd(env),
	)
	return cmd
}

// ctlSimple builds a no-arg control subcommand that enqueues name. reason is an
// optional --reason flag (audit); confirmNeeded gates on --confirm.
func ctlSimple(env *runtimeEnv, use string, name commands.Name, short string, confirmNeeded bool) *cobra.Command {
	var (
		reason  string
		confirm bool
	)
	c := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			token := ""
			if confirmNeeded {
				if !confirm {
					return fmt.Errorf("%s is destructive: pass --confirm to proceed", use)
				}
				token = "confirmed"
			}
			return enqueueCtl(cmd.Context(), env, commands.EnqueueParams{
				Source:      "cli",
				Name:        name,
				Args:        commands.CommandArgs{Reason: reason, ConfirmToken: token},
				RequestedBy: "cli",
			})
		},
	}
	c.Flags().StringVar(&reason, "reason", "", "operator note (audit)")
	if confirmNeeded {
		c.Flags().BoolVar(&confirm, "confirm", false, "confirm this destructive action")
	}
	return c
}

// newSetModeCmd builds `tms ctl set-mode --exec-policy signal|auto [--env
// simu|paper|real] [--confirm]`. The legacy three-valued mode is replaced by
// the 2D model (docs §1.3): exec-policy signal = emit-only; auto + env paper =
// "go paper"; auto + env real = "go live" (both confirm-gated).
func newSetModeCmd(env *runtimeEnv) *cobra.Command {
	var (
		execPolicy string
		envStr     string
		confirm    bool
	)
	c := &cobra.Command{
		Use:   "set-mode --exec-policy signal|auto [--env simu|paper|real]",
		Short: "Switch the live node execution policy/account (auto/live require --confirm)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			exec, err := domain.ParseExecutionPolicy(strings.TrimSpace(execPolicy))
			if err != nil {
				return fmt.Errorf("--exec-policy %q invalid (want signal|auto)", execPolicy)
			}
			var mode string
			if exec == domain.ExecSignal {
				mode = domain.RunWord(exec, "")
			} else {
				e := domain.BrokerEnv(strings.TrimSpace(envStr))
				if e == "" {
					return fmt.Errorf("--exec-policy auto requires --env (paper for paper, real for live)")
				}
				if !e.IsValid() {
					return fmt.Errorf("--env %q invalid (want simu|paper|real)", envStr)
				}
				mode = domain.RunWord(exec, e)
			}
			// auto (paper/live) mutates real risk — confirm-gated.
			token := ""
			if exec == domain.ExecAuto {
				if !confirm {
					return fmt.Errorf("switching to exec-policy=auto (%s) mutates real risk: pass --confirm", mode)
				}
				token = "confirmed"
			}
			return enqueueCtl(cmd.Context(), env, commands.EnqueueParams{
				Source:      "cli",
				Name:        commands.NameSetMode,
				Args:        commands.CommandArgs{Mode: mode, ConfirmToken: token},
				RequestedBy: "cli",
			})
		},
	}
	c.Flags().StringVar(&execPolicy, "exec-policy", "signal", "execution policy: signal (emit-only) | auto (auto-submit)")
	c.Flags().StringVar(&envStr, "env", "", "bound account env for --exec-policy auto: simu | paper | real")
	c.Flags().BoolVar(&confirm, "confirm", false, "confirm an auto (paper/live) switch")
	return c
}

// enqueueCtl opens a pool + Redis and enqueues one command (best-effort notify).
func enqueueCtl(parent context.Context, env *runtimeEnv, p commands.EnqueueParams) error {
	ctx, stop := app.SignalContext(parent)
	defer stop()

	pool, err := db.NewPool(ctx, env.cfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	var rdb *redis.Client
	rdb = redis.NewClient(&redis.Options{
		Addr: env.cfg.RedisAddr, DB: env.cfg.RedisDB, Password: env.cfg.RedisPassword,
	})
	defer func() { _ = rdb.Close() }()
	if rdb.Ping(ctx).Err() != nil {
		_ = rdb.Close()
		rdb = nil // the consumer polls; PG is the durable queue
	}

	enq := commands.NewEnqueuer(pool, rdb, "")
	id, err := enq.Enqueue(ctx, p)
	if err != nil {
		return fmt.Errorf("enqueue %s: %w", p.Name, err)
	}
	env.log.Info().Int64("command_id", id).Str("name", string(p.Name)).Msg("command enqueued")
	fmt.Printf("enqueued command %q (id %d)\n", p.Name, id)
	return nil
}
