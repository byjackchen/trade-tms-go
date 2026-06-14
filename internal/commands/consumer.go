package commands

// consumer.go is the tms-live ops.commands consumer. It claims pending commands
// addressed to the live node (target = "live"), applies each idempotently via a
// Controller, transitions the command row (pending -> acknowledged -> completed
// |rejected) and writes a full tms.audit_log trail.
//
// Claiming is race-free under concurrency (a single live node is the common
// case, but the claim is defensive): an atomic UPDATE ... WHERE status='pending'
// RETURNING moves the row to 'acknowledged' so no two consumers run the same
// command. Re-delivery is harmless: the Controller methods are idempotent.
//
// A Redis notify (PublishNotify) wakes the consumer for low latency; absent
// Redis it falls back to a poll interval, so the control plane works without
// Redis (PG is the durable queue, decision 5/6).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
)

// NotifyChannel is the Redis pub/sub channel the API publishes to after
// enqueuing a live command, waking the consumer immediately.
const NotifyChannel = "tms:live:commands"

// DefaultPollInterval is the consumer's fallback poll cadence when no Redis
// notify arrives (so commands are picked up even with Redis down).
const DefaultPollInterval = 2 * time.Second

// Consumer drains live commands from tms.commands.
type Consumer struct {
	pool      *pgxpool.Pool
	rdb       *redis.Client
	ctrl      Controller
	actor     string
	pollEvery time.Duration
	log       zerolog.Logger
	processed int
}

// ConsumerOptions configures a Consumer.
type ConsumerOptions struct {
	// Pool is the DB pool (required).
	Pool *pgxpool.Pool
	// Redis wakes the consumer on enqueue (optional; nil -> poll only).
	Redis *redis.Client
	// Controller applies commands to the running node (required).
	Controller Controller
	// Actor is the audit actor id for applied commands (e.g. "tms-live:SIGNAL-001").
	Actor string
	// PollInterval overrides DefaultPollInterval.
	PollInterval time.Duration
	// Logger is the structured logger.
	Logger zerolog.Logger
}

// NewConsumer builds a Consumer.
func NewConsumer(opts ConsumerOptions) (*Consumer, error) {
	if opts.Pool == nil {
		return nil, errors.New("commands: nil pool")
	}
	if opts.Controller == nil {
		return nil, errors.New("commands: nil controller")
	}
	poll := opts.PollInterval
	if poll <= 0 {
		poll = DefaultPollInterval
	}
	actor := opts.Actor
	if actor == "" {
		actor = "tms-live"
	}
	return &Consumer{
		pool:      opts.Pool,
		rdb:       opts.Redis,
		ctrl:      opts.Controller,
		actor:     actor,
		pollEvery: poll,
		log:       opts.Logger.With().Str("component", "command-consumer").Logger(),
	}, nil
}

// Processed returns how many commands were applied (telemetry / tests).
func (c *Consumer) Processed() int { return c.processed }

// Run drains pending commands until ctx is canceled. It drains immediately on
// start (recovering any commands enqueued while the node was down), then waits
// on a Redis notify or the poll timer for each subsequent batch. It returns
// when ctx is canceled (graceful) — never on a transient DB/Redis error (those
// are logged and retried).
func (c *Consumer) Run(ctx context.Context) error {
	// Subscribe to the notify channel (best-effort; poll covers Redis outages).
	var msgCh <-chan *redis.Message
	if c.rdb != nil {
		pubsub := c.rdb.Subscribe(ctx, NotifyChannel)
		defer func() { _ = pubsub.Close() }()
		msgCh = pubsub.Channel()
	}

	timer := time.NewTimer(c.pollEvery)
	defer timer.Stop()

	for {
		// Drain everything currently pending (a notify or poll may cover several).
		if err := c.drain(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			c.log.Warn().Err(err).Msg("command drain failed; retrying")
		}
		if ctx.Err() != nil {
			return nil
		}

		// Reset the poll timer and wait for a wake (notify, poll, or shutdown).
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(c.pollEvery)

		select {
		case <-ctx.Done():
			c.log.Info().Msg("command consumer stopped")
			return nil
		case <-timer.C:
		case _, ok := <-msgCh:
			if !ok {
				msgCh = nil // subscription closed; fall back to poll
			}
		}
	}
}

// drain claims and applies every currently-pending live command, oldest first.
func (c *Consumer) drain(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		cmd, ok, err := c.claim(ctx)
		if err != nil {
			return err
		}
		if !ok {
			return nil // none pending
		}
		c.apply(ctx, cmd)
		c.processed++
	}
}

// claim atomically moves the oldest pending live command to 'acknowledged' and
// returns it. The single UPDATE ... WHERE status='pending' ... RETURNING is
// race-free (no two consumers claim the same row).
func (c *Consumer) claim(ctx context.Context) (Command, bool, error) {
	const q = `
UPDATE tms.commands
   SET status = 'acknowledged', acknowledged_at = now()
 WHERE id = (
       SELECT id FROM tms.commands
        WHERE target = $1 AND status = 'pending'
        ORDER BY created_at, id
        FOR UPDATE SKIP LOCKED
        LIMIT 1)
RETURNING id, source, target, name, args, requested_by`

	var (
		cmd     Command
		rawArgs []byte
		nameStr string
	)
	err := c.pool.QueryRow(ctx, q, TargetLive).Scan(
		&cmd.ID, &cmd.Source, &cmd.Target, &nameStr, &rawArgs, &cmd.RequestedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return Command{}, false, nil
	}
	if err != nil {
		return Command{}, false, fmt.Errorf("commands: claim: %w", err)
	}
	cmd.Name = Name(nameStr)
	if len(rawArgs) > 0 {
		if jerr := json.Unmarshal(rawArgs, &cmd.Args); jerr != nil {
			// Malformed args: reject the command (audited) rather than crash.
			c.reject(ctx, cmd, fmt.Sprintf("invalid args json: %v", jerr))
			return Command{}, true, nil
		}
	}
	return cmd, true, nil
}

// apply runs a claimed command via the Controller, then transitions + audits it.
func (c *Consumer) apply(ctx context.Context, cmd Command) {
	if err := cmd.Validate(); err != nil {
		c.reject(ctx, cmd, err.Error())
		return
	}
	var (
		result map[string]any
		err    error
	)
	switch cmd.Name {
	case NameStart, NameResume:
		err = c.ctrl.Resume(ctx)
		result = map[string]any{"emitting": true}
	case NameStop:
		err = c.ctrl.Stop(ctx, cmd.Args.Reason)
		result = map[string]any{"stopped": true}
	case NameHalt:
		err = c.ctrl.Halt(ctx, HaltManual, reasonOr(cmd.Args.Reason, "manual halt"))
		result = map[string]any{"halted": true}
	case NameKill:
		err = c.ctrl.Kill(ctx, reasonOr(cmd.Args.Reason, "kill switch"))
		result = map[string]any{"halted": true, "stopped": true}
	case NameSetMode:
		err = c.ctrl.SetMode(ctx, cmd.Args.Mode)
		result = map[string]any{"mode": cmd.Args.Mode}
	case NameFlatten:
		var n int
		n, err = c.ctrl.Flatten(ctx, reasonOr(cmd.Args.Reason, "flatten"))
		result = map[string]any{"flattened_orders": n}
	case NameEmergencyKill:
		var n int
		n, err = c.ctrl.EmergencyKill(ctx, reasonOr(cmd.Args.Reason, "emergency kill"))
		result = map[string]any{"halted": true, "flattened_orders": n, "stopped": true}
	case NameReconcile:
		var issues bool
		issues, err = c.ctrl.Reconcile(ctx)
		result = map[string]any{"has_issues": issues}
	default:
		c.reject(ctx, cmd, fmt.Sprintf("unhandled command %q", cmd.Name))
		return
	}

	if err != nil {
		c.reject(ctx, cmd, err.Error())
		return
	}
	c.complete(ctx, cmd, result)
}

// complete marks a command completed + writes the audit row.
func (c *Consumer) complete(ctx context.Context, cmd Command, result map[string]any) {
	resBytes, _ := json.Marshal(result)
	if _, err := c.pool.Exec(ctx,
		`UPDATE tms.commands SET status='completed', completed_at=now(), result=$2::jsonb WHERE id=$1`,
		cmd.ID, string(resBytes)); err != nil {
		c.log.Error().Err(err).Int64("command_id", cmd.ID).Msg("marking command completed failed")
	}
	c.audit(ctx, cmd, "completed", result, "")
	c.log.Info().Int64("command_id", cmd.ID).Str("name", string(cmd.Name)).
		Str("requested_by", cmd.RequestedBy).Msg("command applied")
}

// reject marks a command rejected + writes the audit row.
func (c *Consumer) reject(ctx context.Context, cmd Command, reason string) {
	if _, err := c.pool.Exec(ctx,
		`UPDATE tms.commands SET status='rejected', completed_at=now(), error=$2 WHERE id=$1`,
		cmd.ID, reason); err != nil {
		c.log.Error().Err(err).Int64("command_id", cmd.ID).Msg("marking command rejected failed")
	}
	c.audit(ctx, cmd, "rejected", nil, reason)
	c.log.Warn().Int64("command_id", cmd.ID).Str("name", string(cmd.Name)).
		Str("reason", reason).Msg("command rejected")
}

// audit appends a tms.audit_log row (append-only — never updated/deleted).
func (c *Consumer) audit(ctx context.Context, cmd Command, outcome string, result map[string]any, reason string) {
	details := map[string]any{
		"command":      string(cmd.Name),
		"source":       cmd.Source,
		"target":       cmd.Target,
		"requested_by": cmd.RequestedBy,
		"outcome":      outcome,
	}
	if cmd.Args.Mode != "" {
		details["mode"] = cmd.Args.Mode
	}
	if reason != "" {
		details["reason"] = reason
	}
	if result != nil {
		details["result"] = result
	}
	detBytes, _ := json.Marshal(details)
	if _, err := c.pool.Exec(ctx,
		`INSERT INTO tms.audit_log (actor, action, entity, entity_id, details)
		 VALUES ($1, $2, 'command', $3, $4::jsonb)`,
		c.actor, "live.command."+string(cmd.Name), fmt.Sprintf("%d", cmd.ID), string(detBytes)); err != nil {
		c.log.Error().Err(err).Int64("command_id", cmd.ID).Msg("writing audit log failed")
	}
}

func reasonOr(reason, fallback string) string {
	if reason == "" {
		return fallback
	}
	return reason
}

// PublishNotify wakes any live consumer that a new command was enqueued
// (best-effort; the consumer also polls). Called by the API after enqueue.
func PublishNotify(ctx context.Context, rdb *redis.Client) {
	if rdb == nil {
		return
	}
	_ = rdb.Publish(ctx, NotifyChannel, "1").Err()
}
