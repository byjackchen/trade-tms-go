package commands

// enqueue.go is the producer side: the API (and CLI) insert control commands
// into tms.commands and (best-effort) Redis-notify the live consumer. The HTTP
// API is read-only for trading state (api spec §1.1); enqueuing a command is
// the audited side channel (it does not mutate trading state directly — the
// consumer does, under full audit).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

// EnqueueParams is one command-enqueue request.
type EnqueueParams struct {
	// Source is api|cli|ui|system (the origin).
	Source string
	// Name is the control command.
	Name Name
	// Args is the command args.
	Args CommandArgs
	// RequestedBy is the actor (audit; required).
	RequestedBy string
}

// ErrConfirmationRequired is returned when a privileged command (set_mode to
// paper/live) is enqueued without a confirmation token.
var ErrConfirmationRequired = errors.New("commands: confirmation token required for paper/live mode switch")

// Enqueuer inserts commands into tms.commands.
type Enqueuer struct {
	pool        *pgxpool.Pool
	rdb         *redis.Client
	confirmWant string
}

// NewEnqueuer builds an Enqueuer. confirmToken is the expected confirmation
// token for privileged commands (paper/live mode switch); when "" any non-empty
// token in the request is accepted (the API gates by presence). rdb may be nil.
func NewEnqueuer(pool *pgxpool.Pool, rdb *redis.Client, confirmToken string) *Enqueuer {
	return &Enqueuer{pool: pool, rdb: rdb, confirmWant: confirmToken}
}

// Enqueue validates and inserts a command, returning its id. kill/halt/resume/
// stop/start are always allowed; set_mode to paper/live requires a confirmation
// token (api task requirement). The insert is the durable enqueue (PG is truth);
// a Redis notify wakes the consumer for low latency (best-effort).
func (e *Enqueuer) Enqueue(ctx context.Context, p EnqueueParams) (int64, error) {
	if !p.Name.IsValid() {
		return 0, fmt.Errorf("commands: unknown command %q", p.Name)
	}
	if strings.TrimSpace(p.RequestedBy) == "" {
		return 0, errors.New("commands: requested_by is required")
	}
	cmd := Command{Name: p.Name, Args: p.Args}
	if err := cmd.Validate(); err != nil {
		return 0, err
	}
	if RequiresConfirmation(p.Name, p.Args.Mode) {
		if !e.tokenOK(p.Args.ConfirmToken) {
			return 0, ErrConfirmationRequired
		}
	}
	source := p.Source
	if source == "" {
		source = "api"
	}

	// Do not persist the confirmation token (no secrets in the durable row).
	storeArgs := p.Args
	storeArgs.ConfirmToken = ""
	argBytes, err := json.Marshal(storeArgs)
	if err != nil {
		return 0, fmt.Errorf("commands: marshal args: %w", err)
	}

	var id int64
	if err := e.pool.QueryRow(ctx,
		`INSERT INTO tms.commands (source, target, name, args, requested_by)
		 VALUES ($1, $2, $3, $4::jsonb, $5) RETURNING id`,
		source, TargetLive, string(p.Name), string(argBytes), p.RequestedBy,
	).Scan(&id); err != nil {
		return 0, fmt.Errorf("commands: enqueue: %w", err)
	}

	PublishNotify(ctx, e.rdb)
	return id, nil
}

// tokenOK reports whether token satisfies the confirmation requirement. When a
// specific token is configured it must match exactly; otherwise any non-empty
// token is accepted (presence gate).
func (e *Enqueuer) tokenOK(token string) bool {
	token = strings.TrimSpace(token)
	if e.confirmWant != "" {
		return token == e.confirmWant
	}
	return token != ""
}
