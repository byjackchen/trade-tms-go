package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/byjackchen/trade-tms-go/internal/db"
	"github.com/byjackchen/trade-tms-go/internal/jobs"
)

// newJobsCmd declares the `tms jobs` ops surface over the durable queue:
// enqueue / cancel / show / list. It exists so operators (and acceptance
// tests) can drive the worker without an API; trading mutations stay out
// of HTTP by design (api spec §1.1).
func newJobsCmd(env *runtimeEnv) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "jobs",
		Short: "Inspect and control the durable job queue (tms.jobs)",
	}
	cmd.AddCommand(
		newJobsEnqueueCmd(env),
		newJobsCancelCmd(env),
		newJobsShowCmd(env),
		newJobsListCmd(env),
	)
	return cmd
}

// withQueue builds a Queue for one CLI invocation (no Redis notifier: CLI
// state changes still audit-log; live-UI events come from worker activity).
func withQueue(cmd *cobra.Command, env *runtimeEnv, fn func(q *jobs.Queue) error) error {
	ctx := cmd.Context()
	pool, err := db.NewPool(ctx, env.cfg)
	if err != nil {
		return err
	}
	defer pool.Close()
	q, err := jobs.NewQueue(pool, env.log)
	if err != nil {
		return err
	}
	return fn(q)
}

func newJobsEnqueueCmd(env *runtimeEnv) *cobra.Command {
	var (
		payloadStr  string
		dedupeKey   string
		priority    int32
		runAtStr    string
		maxAttempts int32
	)
	cmd := &cobra.Command{
		Use:   "enqueue <kind>",
		Short: "Enqueue a job (payload is a JSON object)",
		Example: `  tms jobs enqueue data.refresh --payload '{"source":"parquet","tables":["sep"]}' \
      --dedupe-key data.refresh:sep --max-attempts 3`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var payload json.RawMessage = []byte("{}")
			if strings.TrimSpace(payloadStr) != "" {
				if !json.Valid([]byte(payloadStr)) {
					return fmt.Errorf("--payload is not valid JSON: %q", payloadStr)
				}
				payload = json.RawMessage(payloadStr)
			}
			var runAt time.Time
			if runAtStr != "" {
				t, err := time.Parse(time.RFC3339, runAtStr)
				if err != nil {
					return fmt.Errorf("invalid --run-at %q (want RFC3339): %w", runAtStr, err)
				}
				runAt = t
			}
			return withQueue(cmd, env, func(q *jobs.Queue) error {
				job, deduped, err := q.Enqueue(cmd.Context(), jobs.EnqueueParams{
					Kind:        args[0],
					Payload:     payload,
					DedupeKey:   dedupeKey,
					Priority:    priority,
					RunAt:       runAt,
					MaxAttempts: maxAttempts,
					Actor:       "cli",
				})
				if err != nil {
					return err
				}
				if deduped {
					fmt.Fprintf(cmd.OutOrStdout(),
						"deduped: active job %d (%s, status=%s) already holds dedupe key %q\n",
						job.ID, job.Kind, job.Status, dedupeKey)
					return nil
				}
				fmt.Fprintf(cmd.OutOrStdout(), "enqueued job %d (%s), run_at=%s\n",
					job.ID, job.Kind, job.RunAt.UTC().Format(time.RFC3339))
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&payloadStr, "payload", "{}", "JSON object payload for the handler")
	cmd.Flags().StringVar(&dedupeKey, "dedupe-key", "", "at most one active (queued|running) job per key")
	cmd.Flags().Int32Var(&priority, "priority", 0, "claim priority (higher first)")
	cmd.Flags().StringVar(&runAtStr, "run-at", "", "earliest execution time (RFC3339; default now)")
	cmd.Flags().Int32Var(&maxAttempts, "max-attempts", 1, "total attempts before terminal failure")
	return cmd
}

func newJobsCancelCmd(env *runtimeEnv) *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use:   "cancel <id>",
		Short: "Cancel a job (queued: immediate; running: cooperative)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("invalid job id %q: %w", args[0], err)
			}
			return withQueue(cmd, env, func(q *jobs.Queue) error {
				outcome, job, err := q.Cancel(cmd.Context(), id, "cli", reason)
				if err != nil {
					return err
				}
				switch outcome {
				case jobs.CancelDone:
					fmt.Fprintf(cmd.OutOrStdout(), "job %d canceled\n", job.ID)
				case jobs.CancelRequested:
					fmt.Fprintf(cmd.OutOrStdout(),
						"job %d is running; cancel requested — the worker will stop it cooperatively\n", job.ID)
				case jobs.CancelAlreadyTerminal:
					fmt.Fprintf(cmd.OutOrStdout(), "job %d already finished (status=%s); nothing to do\n",
						job.ID, job.Status)
				}
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "cancellation reason for the audit trail")
	return cmd
}

func newJobsShowCmd(env *runtimeEnv) *cobra.Command {
	return &cobra.Command{
		Use:   "show <id>",
		Short: "Print one job as JSON",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("invalid job id %q: %w", args[0], err)
			}
			return withQueue(cmd, env, func(q *jobs.Queue) error {
				job, err := q.Get(cmd.Context(), id)
				if err != nil {
					return err
				}
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(job)
			})
		},
	}
}

func newJobsListCmd(env *runtimeEnv) *cobra.Command {
	var (
		kind   string
		status string
		limit  int32
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List jobs newest-first",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withQueue(cmd, env, func(q *jobs.Queue) error {
				list, err := q.List(cmd.Context(), jobs.ListFilter{
					Kind:   kind,
					Status: jobs.Status(status),
					Limit:  limit,
				})
				if err != nil {
					return err
				}
				out := cmd.OutOrStdout()
				fmt.Fprintf(out, "%-8s %-24s %-10s %-8s %-20s %s\n",
					"id", "kind", "status", "attempt", "updated", "error")
				for _, j := range list {
					lastErr := ""
					if j.LastError != nil {
						lastErr = *j.LastError
						if i := strings.IndexByte(lastErr, '\n'); i >= 0 {
							lastErr = lastErr[:i]
						}
						if len(lastErr) > 60 {
							lastErr = lastErr[:60] + "…"
						}
					}
					fmt.Fprintf(out, "%-8d %-24s %-10s %d/%-6d %-20s %s\n",
						j.ID, j.Kind, j.Status, j.Attempts, j.MaxAttempts,
						j.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"), lastErr)
				}
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&kind, "kind", "", "filter by kind")
	cmd.Flags().StringVar(&status, "status", "", "filter by status (queued|running|succeeded|failed|canceled)")
	cmd.Flags().Int32Var(&limit, "limit", 50, "max rows")
	return cmd
}
