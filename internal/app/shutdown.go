package app

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rs/zerolog"
)

// SignalContext returns a context cancelled on SIGINT/SIGTERM, plus its
// stop function. Every long-running subcommand (api, live, worker) derives
// its lifecycle from this context; a second signal while shutting down
// falls through to the default handler and kills the process — the
// standard "graceful first, forceful second" contract.
func SignalContext(parent context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
}

// ShutdownFunc is one cleanup step (server drain, pool close, stream flush).
type ShutdownFunc struct {
	Name string
	Fn   func(context.Context) error
}

// GracefulShutdown runs the given steps in order, each bounded by the
// shared timeout, logging progress and collecting every error rather than
// stopping at the first one — a half-finished shutdown should still try to
// close everything it can. Returns the joined errors (nil when all clean).
func GracefulShutdown(log zerolog.Logger, timeout time.Duration, steps ...ShutdownFunc) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var errs []error
	for _, step := range steps {
		log.Info().Str("step", step.Name).Msg("shutdown: running step")
		if err := step.Fn(ctx); err != nil {
			log.Error().Err(err).Str("step", step.Name).Msg("shutdown: step failed")
			errs = append(errs, err)
			continue
		}
		log.Info().Str("step", step.Name).Msg("shutdown: step complete")
	}
	return errors.Join(errs...)
}
