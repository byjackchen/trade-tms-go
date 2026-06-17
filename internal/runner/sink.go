package runner

// sink.go adapts the publish layer (PG store + Redis publisher) to the
// livengine.SignalSink + StateEmitter seam. It is the Build2 sink (signal.go's
// "DB-upsert + Redis-publish sink") wiring:
//
//   - EmitSignal: normalize the strategy signal -> for each per-name signal,
//     persist to tms.signals (append for live, UPSERT for EOD) AND
//     publish a SignalUpdate to Redis.
//   - EmitState:  publish each strategy's state_summary as a StrategyStateUpdate.
//   - EmitHealth: publish a PortfolioHealthUpdate (+ an empty-book position
//     snapshot, signal mode) to Redis.
//
// Persistence (PG) is the gate: a DB failure aborts the run (we must not lose a
// signal). Redis is transport-only (decision 5): a publish failure is logged
// and the run continues (the cockpit reconstructs from PG).

import (
	"context"
	"time"

	"github.com/rs/zerolog"

	"github.com/byjackchen/trade-tms-go/internal/livengine"
	"github.com/byjackchen/trade-tms-go/internal/publish"
)

// SinkMode selects the persistence semantics.
type SinkMode int

const (
	// SinkAppend is the streaming live path: append-only rows (as_of NULL).
	SinkAppend SinkMode = iota
	// SinkUpsert is the EOD engine-replay path: idempotent UPSERT on
	// (strategy_id, symbol, as_of).
	SinkUpsert
)

// Sink implements livengine.SignalSink + livengine.StateEmitter over the
// publish layer.
type Sink struct {
	store     *publish.Store
	publisher *publish.Publisher
	log       zerolog.Logger
	mode      SinkMode

	// sessionID owns the persisted rows (nil for detached EOD).
	sessionID *int64
	// asOf is the EOD refresh date (non-nil only for SinkUpsert).
	asOf *time.Time

	// counters (telemetry / tests).
	intentRows  int
	publishErrs int
}

// SinkOptions configures a Sink.
type SinkOptions struct {
	// Store persists intents (required for the live/EOD durable path; may be nil
	// for a Redis-only dry run, in which case persistence is skipped).
	Store *publish.Store
	// Publisher fans updates to Redis (may be a no-op publisher / nil).
	Publisher *publish.Publisher
	// Mode selects append vs upsert.
	Mode SinkMode
	// SessionID owns persisted rows (nil for detached EOD).
	SessionID *int64
	// AsOf is the EOD refresh date (required for SinkUpsert).
	AsOf *time.Time
	// Logger is the structured logger.
	Logger zerolog.Logger
}

// NewSink builds a Sink.
func NewSink(opts SinkOptions) *Sink {
	return &Sink{
		store:     opts.Store,
		publisher: opts.Publisher,
		log:       opts.Logger.With().Str("component", "live-sink").Logger(),
		mode:      opts.Mode,
		sessionID: opts.SessionID,
		asOf:      opts.AsOf,
	}
}

// SignalRows returns how many signal-intent rows were persisted (telemetry).
func (s *Sink) SignalRows() int { return s.intentRows }

// PublishErrors returns how many Redis publish calls failed (best-effort
// transport; never aborts the run).
func (s *Sink) PublishErrors() int { return s.publishErrs }

// EmitSignal normalizes the strategy signal into per-name NormalizedSignals,
// persists each (PG: the gate) and publishes each (Redis: best-effort).
func (s *Sink) EmitSignal(ctx context.Context, rec livengine.SignalRecord) error {
	norms, err := publish.NormalizeSignal(rec.Payload)
	if err != nil {
		return err
	}
	tsEventNS := rec.AsOf.UTC().UnixNano()
	for _, n := range norms {
		row := publish.IntentRow{
			Norm:      n,
			SessionID: s.sessionID,
			AsOf:      s.asOf,
			TSEvent:   rec.AsOf,
		}
		if s.store != nil {
			var perr error
			if s.mode == SinkUpsert {
				perr = s.store.Upsert(ctx, row)
			} else {
				perr = s.store.Append(ctx, row)
			}
			if perr != nil {
				return perr // persistence is the gate
			}
			s.intentRows++
		}
		if err := s.publisher.PublishSignal(ctx, n, tsEventNS); err != nil {
			s.publishErrs++
			s.log.Warn().Err(err).Str("strategy_id", n.StrategyID).Str("symbol", n.Symbol).
				Msg("redis publish signal intent failed; PG row is durable")
		}
	}
	return nil
}

// EmitState publishes a strategy's state_summary as a StrategyStateUpdate
// (best-effort transport; state summaries are observability, not durable
// truth, so a publish failure is logged and swallowed).
func (s *Sink) EmitState(ctx context.Context, rec livengine.StateRecord) error {
	tsEventNS := rec.AsOf.UTC().UnixNano()
	if err := s.publisher.PublishStrategyState(ctx, rec.StrategyID, rec.Summary, tsEventNS); err != nil {
		s.publishErrs++
		s.log.Warn().Err(err).Str("strategy_id", rec.StrategyID).Msg("redis publish strategy state failed")
	}
	return nil
}

// EmitHealth publishes a PortfolioHealthUpdate + the signal-mode empty position
// book (best-effort transport).
func (s *Sink) EmitHealth(ctx context.Context, rec livengine.HealthRecord) error {
	tsEventNS := rec.AsOf.UTC().UnixNano()
	env := publish.PortfolioHealthEnvelope{
		DayPnL:           rec.Snapshot.DayPnLFloat(),
		DayPnLPct:        rec.Snapshot.DayPnLPctFloat(),
		DailyLossHalt:    rec.Snapshot.IsDailyLossHalt(),
		HaltHeadroomPct:  rec.Snapshot.HaltHeadroomPctFloat(),
		ConcentrationPct: rec.Snapshot.ConcentrationPctFloat(),
		TSEvent:          tsEventNS,
	}
	if err := s.publisher.PublishPortfolioHealth(ctx, env); err != nil {
		s.publishErrs++
		s.log.Warn().Err(err).Msg("redis publish portfolio health failed")
	}
	if err := s.publisher.PublishEmptyPositions(ctx, tsEventNS); err != nil {
		s.publishErrs++
		s.log.Warn().Err(err).Msg("redis publish empty positions failed")
	}
	return nil
}

// compile-time checks: Sink satisfies both live seams.
var (
	_ livengine.SignalSink   = (*Sink)(nil)
	_ livengine.StateEmitter = (*Sink)(nil)
)
