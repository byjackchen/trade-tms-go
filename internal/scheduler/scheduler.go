package scheduler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
)

// Defaults for the scheduler loop.
const (
	// DefaultDailyAt is the wall-clock time of day (in the scheduler TZ) the
	// daily pipeline fires when unconfigured: ~2.5h after the 16:00 ET close.
	DefaultDailyAt = "18:30"
	// DefaultTZ is the time zone DailyAt is interpreted in.
	DefaultTZ = "America/New_York"
	// DefaultTick is how often the loop wakes to re-evaluate "should the day's
	// pipeline fire now?". A minute is ample: the fire decision is a cheap
	// idempotent claim, and a missed exact instant simply fires on the next
	// tick (still the same trading date).
	DefaultTick = 1 * time.Minute
)

// TimeOfDay is a wall-clock HH:MM within a day.
type TimeOfDay struct {
	Hour   int
	Minute int
}

// ParseTimeOfDay parses "HH:MM" (24-hour). It rejects out-of-range and
// malformed input loudly so a typo in TMS_SCHEDULER_DAILY_AT fails at startup
// rather than silently never firing.
func ParseTimeOfDay(s string) (TimeOfDay, error) {
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil {
		return TimeOfDay{}, fmt.Errorf("scheduler: invalid time-of-day %q (want HH:MM, 24-hour): %w", s, err)
	}
	return TimeOfDay{Hour: t.Hour(), Minute: t.Minute()}, nil
}

// String renders the time as "HH:MM".
func (t TimeOfDay) String() string { return fmt.Sprintf("%02d:%02d", t.Hour, t.Minute) }

// Options configures a Scheduler.
type Options struct {
	// DailyAt is the wall-clock fire time within Loc. Zero value parses from
	// DefaultDailyAt at New time.
	DailyAt TimeOfDay
	// Loc is the time zone DailyAt is interpreted in (defaults to the
	// calendar's America/New_York location when nil).
	Loc *time.Location
	// Catchup enables the startup catch-up (default behaviour is set by the
	// caller; the zero value is false, so cmd passes config.SchedulerCatchup).
	Catchup bool
	// Tick overrides the loop cadence (tests). Zero = DefaultTick.
	Tick time.Duration
	// InstanceID identifies this scheduler in the ledger's claimed_by column
	// (host/pid). "" = "scheduler".
	InstanceID string
	// Now overrides the clock (tests). nil = time.Now.
	Now func() time.Time
}

// Scheduler enqueues the daily data pipeline on each NYSE trading day at the
// configured time, idempotently and single-leader via the Ledger. It is safe
// for one instance to run per process; multiple instances cooperate through
// the ledger's per-day claim.
type Scheduler struct {
	cal      *calendar.Calendar
	queue    Enqueuer
	ledger   Ledger
	log      zerolog.Logger
	dailyAt  TimeOfDay
	loc      *time.Location
	catchup  bool
	tick     time.Duration
	instance string
	now      func() time.Time
}

// New builds a Scheduler. cal, queue and ledger are required.
func New(cal *calendar.Calendar, queue Enqueuer, ledger Ledger, log zerolog.Logger, opts Options) (*Scheduler, error) {
	if cal == nil {
		return nil, errors.New("scheduler: nil calendar")
	}
	if queue == nil {
		return nil, errors.New("scheduler: nil job queue")
	}
	if ledger == nil {
		return nil, errors.New("scheduler: nil ledger")
	}
	dailyAt := opts.DailyAt
	if dailyAt == (TimeOfDay{}) {
		var err error
		if dailyAt, err = ParseTimeOfDay(DefaultDailyAt); err != nil {
			return nil, err
		}
	}
	loc := opts.Loc
	if loc == nil {
		loc = cal.Location()
	}
	tick := opts.Tick
	if tick <= 0 {
		tick = DefaultTick
	}
	instance := strings.TrimSpace(opts.InstanceID)
	if instance == "" {
		instance = "scheduler"
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Scheduler{
		cal:      cal,
		queue:    queue,
		ledger:   ledger,
		log:      log.With().Str("component", "scheduler").Logger(),
		dailyAt:  dailyAt,
		loc:      loc,
		catchup:  opts.Catchup,
		tick:     tick,
		instance: instance,
		now:      now,
	}, nil
}

// fireInstant returns the UTC instant the daily pipeline should fire ON the
// trading date d (DailyAt wall-clock in the scheduler TZ). Across DST this
// tracks the local wall clock, which is what an operator configuring "18:30
// New York" expects.
func (s *Scheduler) fireInstant(d calendar.Date) time.Time {
	return time.Date(d.Year, d.Month, d.Day, s.dailyAt.Hour, s.dailyAt.Minute, 0, 0, s.loc)
}

// tradingDate returns the NYSE session date for the wall-clock instant now in
// the scheduler TZ, and whether that date is a trading day. A date outside the
// calendar's supported range yields an error (the caller logs and skips).
func (s *Scheduler) tradingDate(now time.Time) (calendar.Date, bool, error) {
	d := calendar.DateOf(now, s.loc)
	trading, err := s.cal.IsTradingDay(d)
	if err != nil {
		return d, false, err
	}
	return d, trading, nil
}

// TickResult describes what one evaluation did (returned for tests/logging).
type TickResult struct {
	// Date is the trading date evaluated.
	Date calendar.Date
	// Action summarizes the decision.
	Action TickAction
	// Pipeline is set when Action == ActionEnqueued.
	Pipeline PipelineResult
}

// TickAction enumerates a tick's decision.
type TickAction string

// Tick actions.
const (
	// ActionSkippedNonTradingDay: weekend/holiday — nothing to do.
	ActionSkippedNonTradingDay TickAction = "skipped_non_trading_day"
	// ActionWaiting: trading day, but the fire time has not arrived yet.
	ActionWaiting TickAction = "waiting"
	// ActionEnqueued: this tick won the day's slot and enqueued the pipeline.
	ActionEnqueued TickAction = "enqueued"
	// ActionAlreadyDone: the fire time has passed but the day's slot was
	// already claimed (another instance / an earlier tick / a manual force).
	ActionAlreadyDone TickAction = "already_done"
	// ActionOutOfRange: the date falls outside the calendar's supported range.
	ActionOutOfRange TickAction = "out_of_range"
)

// evaluate runs one scheduling decision for the wall-clock instant now. It is
// the pure, testable heart of the loop: given a clock reading it decides
// whether to enqueue the day's pipeline and (when it wins the ledger slot)
// does so. trig distinguishes the on-time path from startup catch-up.
func (s *Scheduler) evaluate(ctx context.Context, now time.Time, trig Trigger) (TickResult, error) {
	d, trading, err := s.tradingDate(now)
	if err != nil {
		if errors.Is(err, calendar.ErrOutOfRange) {
			return TickResult{Date: d, Action: ActionOutOfRange}, nil
		}
		return TickResult{Date: d}, err
	}
	if !trading {
		return TickResult{Date: d, Action: ActionSkippedNonTradingDay}, nil
	}
	if now.Before(s.fireInstant(d)) {
		return TickResult{Date: d, Action: ActionWaiting}, nil
	}

	// Fire time has arrived/passed on a trading day: attempt to claim the slot.
	claim, err := s.ledger.Claim(ctx, PipelineDaily, d, s.instance, trig)
	if err != nil {
		return TickResult{Date: d}, err
	}
	if !claim.Won {
		return TickResult{Date: d, Action: ActionAlreadyDone}, nil
	}

	pipe, err := enqueuePipeline(ctx, s.queue, d, now, "scheduler")
	if err != nil {
		// The slot is claimed but enqueue partly failed; do not silently lose
		// the day. We keep the claim (re-enqueuing would double-run) and
		// surface the error — the worker's data.refresh, if it landed, still
		// runs; the operator can force eod via `tms sync now` if needed.
		return TickResult{Date: d, Action: ActionEnqueued, Pipeline: pipe}, err
	}
	if rerr := s.ledger.RecordJobs(ctx, claim.RunID, pipe.DataJobID, pipe.EODJobID); rerr != nil {
		// Traceability only — the pipeline is enqueued; log and continue.
		s.log.Warn().Err(rerr).Int64("run_id", claim.RunID).
			Msg("scheduler: recording pipeline job ids failed (pipeline still enqueued)")
	}
	s.log.Info().
		Str("trading_date", d.String()).
		Str("trigger", string(trig)).
		Int64("data_job_id", pipe.DataJobID).
		Int64("eod_job_id", pipe.EODJobID).
		Bool("data_deduped", pipe.DataDeduped).
		Bool("eod_deduped", pipe.EODDeduped).
		Msg("daily pipeline enqueued")
	return TickResult{Date: d, Action: ActionEnqueued, Pipeline: pipe}, nil
}

// Tick performs one on-time scheduling evaluation at the current clock. It is
// exported so the loop and tests share the same entry point.
func (s *Scheduler) Tick(ctx context.Context) (TickResult, error) {
	return s.evaluate(ctx, s.now(), TriggerScheduled)
}

// SyncNow forces the daily pipeline immediately, regardless of the configured
// fire time, on the current NYSE trading date. It backs `tms sync now` /
// POST /api/v1/data/sync-now. On a non-trading day it targets the most recent
// prior session (so a weekend force still refreshes the last real session).
// It is idempotent through the same ledger slot: a second force for an
// already-claimed day is a no-op (ActionAlreadyDone).
func (s *Scheduler) SyncNow(ctx context.Context, actor string) (TickResult, error) {
	now := s.now()
	d := calendar.DateOf(now, s.loc)
	trading, err := s.cal.IsTradingDay(d)
	if err != nil {
		return TickResult{Date: d}, err
	}
	if !trading {
		sess, perr := s.cal.PrevSession(d)
		if perr != nil {
			return TickResult{Date: d}, fmt.Errorf("scheduler: sync-now: no prior session before %s: %w", d, perr)
		}
		d = sess.Date
	}

	claim, err := s.ledger.Claim(ctx, PipelineDaily, d, s.instance, TriggerManual)
	if err != nil {
		return TickResult{Date: d}, err
	}
	if !claim.Won {
		return TickResult{Date: d, Action: ActionAlreadyDone}, nil
	}
	pipe, err := enqueuePipeline(ctx, s.queue, d, now, actorOrScheduler(actor))
	if err != nil {
		return TickResult{Date: d, Action: ActionEnqueued, Pipeline: pipe}, err
	}
	if rerr := s.ledger.RecordJobs(ctx, claim.RunID, pipe.DataJobID, pipe.EODJobID); rerr != nil {
		s.log.Warn().Err(rerr).Int64("run_id", claim.RunID).Msg("scheduler: recording sync-now job ids failed")
	}
	s.log.Info().Str("trading_date", d.String()).Str("actor", actorOrScheduler(actor)).
		Int64("data_job_id", pipe.DataJobID).Int64("eod_job_id", pipe.EODJobID).
		Msg("sync-now: daily pipeline forced")
	return TickResult{Date: d, Action: ActionEnqueued, Pipeline: pipe}, nil
}

func actorOrScheduler(actor string) string {
	if a := strings.TrimSpace(actor); a != "" {
		return a
	}
	return "scheduler"
}

// Run drives the scheduling loop until ctx is canceled. It performs the
// startup catch-up (when enabled) once, then evaluates on every tick. The loop
// never sleeps on the wall clock beyond the ticker, so SIGTERM cancellation is
// immediate. Run returns nil on clean shutdown (ctx canceled).
func (s *Scheduler) Run(ctx context.Context) error {
	s.log.Info().
		Str("daily_at", s.dailyAt.String()).
		Str("tz", s.loc.String()).
		Bool("catchup", s.catchup).
		Dur("tick", s.tick).
		Str("instance", s.instance).
		Msg("scheduler starting")

	if s.catchup {
		if _, err := s.evaluate(ctx, s.now(), TriggerCatchup); err != nil {
			// A catch-up failure must not crash the scheduler — log and carry
			// on into the steady-state loop (the day's slot is either claimed
			// or will be retried on the next tick).
			s.log.Error().Err(err).Msg("scheduler: startup catch-up failed; continuing")
		}
	}

	ticker := time.NewTicker(s.tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.log.Info().Msg("scheduler stopping (context canceled)")
			return nil
		case <-ticker.C:
			if _, err := s.Tick(ctx); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				s.log.Error().Err(err).Msg("scheduler: tick failed; will retry next tick")
			}
		}
	}
}
