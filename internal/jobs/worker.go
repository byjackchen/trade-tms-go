package jobs

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"runtime/debug"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
)

// Worker defaults; all overridable via WorkerOptions.
const (
	DefaultConcurrency       = 4
	DefaultPollInterval      = 2 * time.Second
	DefaultHeartbeatInterval = 5 * time.Second
	DefaultStaleAfter        = 60 * time.Second
	DefaultReapInterval      = 15 * time.Second
	DefaultDrainTimeout      = 30 * time.Second
	// finalizeTimeout bounds the terminal-state write after a handler
	// returns; it uses a context detached from shutdown so terminal states
	// land even mid-drain.
	finalizeTimeout = 15 * time.Second
)

// WorkerOptions configures a Worker pool. Zero values take the defaults
// above.
type WorkerOptions struct {
	// ID identifies this worker in claimed_by / audit rows.
	// "" = "<hostname>-<pid>".
	ID string
	// Concurrency is the number of parallel job executors.
	Concurrency int
	// PollInterval is the idle claim-poll cadence (±20% jitter to
	// de-synchronize fleets).
	PollInterval time.Duration
	// HeartbeatInterval is the per-running-job liveness cadence. Must be
	// comfortably below StaleAfter (validated: at most StaleAfter/3).
	HeartbeatInterval time.Duration
	// StaleAfter is the heartbeat TTL after which other workers' reapers
	// may recover a running job.
	StaleAfter time.Duration
	// ReapInterval is the stale-claim reaper cadence.
	ReapInterval time.Duration
	// DrainTimeout bounds graceful drain: after shutdown begins, in-flight
	// handlers get this long to finish before their contexts are canceled
	// and the jobs are released back to the queue.
	DrainTimeout time.Duration
}

func (o *WorkerOptions) applyDefaults() error {
	if o.ID == "" {
		host, err := os.Hostname()
		if err != nil || host == "" {
			host = "worker"
		}
		o.ID = host + "-" + strconv.Itoa(os.Getpid())
	}
	if o.Concurrency == 0 {
		o.Concurrency = DefaultConcurrency
	}
	if o.PollInterval == 0 {
		o.PollInterval = DefaultPollInterval
	}
	if o.HeartbeatInterval == 0 {
		o.HeartbeatInterval = DefaultHeartbeatInterval
	}
	if o.StaleAfter == 0 {
		o.StaleAfter = DefaultStaleAfter
	}
	if o.ReapInterval == 0 {
		o.ReapInterval = DefaultReapInterval
	}
	if o.DrainTimeout == 0 {
		o.DrainTimeout = DefaultDrainTimeout
	}
	switch {
	case o.Concurrency < 1:
		return fmt.Errorf("jobs: worker concurrency must be >= 1, got %d", o.Concurrency)
	case o.PollInterval <= 0 || o.HeartbeatInterval <= 0 || o.StaleAfter <= 0 ||
		o.ReapInterval <= 0 || o.DrainTimeout <= 0:
		return errors.New("jobs: worker intervals must be > 0")
	case o.HeartbeatInterval > o.StaleAfter/3:
		return fmt.Errorf("jobs: heartbeat interval %s must be <= stale TTL/3 (%s) or live jobs get reaped",
			o.HeartbeatInterval, o.StaleAfter/3)
	}
	return nil
}

// Worker runs a pool of concurrent job executors plus a stale-claim reaper.
// Lifecycle: Run(ctx) blocks until ctx is canceled, then drains — claiming
// stops immediately, in-flight handlers get DrainTimeout to finish, then
// their contexts are canceled and the jobs are released back to the queue
// for another worker. Each job runs panic-isolated: a handler panic fails
// that job (stack captured in last_error) and the executor goroutine
// survives.
type Worker struct {
	queue    *Queue
	registry *Registry
	log      zerolog.Logger
	opts     WorkerOptions

	inFlight atomic.Int64
	started  atomic.Bool
}

// NewWorker validates options and builds a Worker.
func NewWorker(queue *Queue, registry *Registry, log zerolog.Logger, opts WorkerOptions) (*Worker, error) {
	if queue == nil {
		return nil, errors.New("jobs: nil queue")
	}
	if registry == nil {
		return nil, errors.New("jobs: nil registry")
	}
	if err := opts.applyDefaults(); err != nil {
		return nil, err
	}
	return &Worker{
		queue:    queue,
		registry: registry,
		log:      log.With().Str("component", "jobs-worker").Str("worker_id", opts.ID).Logger(),
		opts:     opts,
	}, nil
}

// ID returns the worker identity used in claimed_by.
func (w *Worker) ID() string { return w.opts.ID }

// InFlight returns the number of jobs currently executing (health surface).
func (w *Worker) InFlight() int { return int(w.inFlight.Load()) }

// Started reports whether Run has begun (health surface).
func (w *Worker) Started() bool { return w.started.Load() }

// Run starts the executor pool and reaper, blocking until ctx is canceled
// and all in-flight jobs are settled (finished, canceled or released).
// Always returns nil after a clean drain — shutdown via signal is not an
// error.
func (w *Worker) Run(ctx context.Context) error {
	w.started.Store(true)
	w.log.Info().
		Int("concurrency", w.opts.Concurrency).
		Dur("poll_interval", w.opts.PollInterval).
		Dur("heartbeat_interval", w.opts.HeartbeatInterval).
		Dur("stale_after", w.opts.StaleAfter).
		Dur("drain_timeout", w.opts.DrainTimeout).
		Strs("handlers", w.registry.Kinds()).
		Msg("worker starting")

	var wg sync.WaitGroup
	for i := 0; i < w.opts.Concurrency; i++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			w.claimLoop(ctx, slot)
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.reapLoop(ctx)
	}()

	wg.Wait()
	w.log.Info().Msg("worker drained; all executors stopped")
	return nil
}

// claimLoop is one executor: claim → execute → repeat, idling on the
// jittered poll interval when the queue is empty.
func (w *Worker) claimLoop(ctx context.Context, slot int) {
	log := w.log.With().Int("slot", slot).Logger()
	for {
		if ctx.Err() != nil {
			return
		}
		job, err := w.queue.Claim(ctx, w.opts.ID)
		switch {
		case errors.Is(err, ErrNoJob):
			w.sleep(ctx, w.jitter(w.opts.PollInterval))
		case err != nil:
			if ctx.Err() != nil {
				return
			}
			log.Warn().Err(err).Msg("claim failed; backing off")
			w.sleep(ctx, w.jitter(w.opts.PollInterval))
		default:
			w.execute(ctx, job)
		}
	}
}

// reapLoop periodically recovers stale claims left by dead workers.
func (w *Worker) reapLoop(ctx context.Context) {
	ticker := time.NewTicker(w.opts.ReapInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			stats, err := w.queue.ReapStale(ctx, w.opts.StaleAfter)
			if err != nil && ctx.Err() == nil {
				w.log.Warn().Err(err).Msg("stale-claim reap failed; will retry")
			}
			if stats.Total() > 0 {
				w.log.Info().
					Int("requeued", stats.Requeued).
					Int("failed", stats.Failed).
					Int("canceled", stats.Canceled).
					Msg("recovered stale claims")
			}
		}
	}
}

// execState carries cross-goroutine flags for one job execution.
type execState struct {
	lost            atomic.Bool // claim lost: write no terminal state
	cancelRequested atomic.Bool // operator cancel observed
	drainExpired    atomic.Bool // shutdown outlived DrainTimeout
}

// execute runs one claimed job to a settled state. The handler context is
// detached from the shutdown context: SIGTERM does not cancel handlers
// directly — the drain watcher cancels them only after DrainTimeout, and
// the job is then released back to the queue.
func (w *Worker) execute(parentCtx context.Context, job *Job) {
	w.inFlight.Add(1)
	defer w.inFlight.Add(-1)

	log := w.log.With().Int64("job_id", job.ID).Str("kind", job.Kind).
		Int32("attempt", job.Attempts).Logger()
	log.Info().Msg("job started")
	started := time.Now()

	jobCtx, cancelJob := context.WithCancel(context.WithoutCancel(parentCtx))
	defer cancelJob()

	state := &execState{}
	done := make(chan struct{})
	var watchers sync.WaitGroup
	watchers.Add(2)
	go func() {
		defer watchers.Done()
		w.heartbeatLoop(parentCtx, job, state, cancelJob, done, log)
	}()
	go func() {
		defer watchers.Done()
		w.drainWatch(parentCtx, state, cancelJob, done, log)
	}()

	report := func(pctx context.Context, progress any) error {
		cr, err := w.queue.ReportProgress(pctx, job.ID, w.opts.ID, progress)
		if errors.Is(err, ErrLostClaim) {
			state.lost.Store(true)
			cancelJob()
			return err
		}
		if err != nil {
			return err // informational; handler keeps running
		}
		if cr && !state.cancelRequested.Swap(true) {
			log.Info().Msg("cancel requested (via progress report); canceling handler context")
			cancelJob()
		}
		return nil
	}

	result, err := w.runHandler(jobCtx, job, report)
	close(done)
	watchers.Wait()

	w.finalize(parentCtx, job, state, result, err, log, time.Since(started))
}

// heartbeatLoop bumps the job heartbeat and relays cooperative-cancel /
// lost-claim signals. It keeps beating after a cancel request so a slowly
// winding-down handler is not reaped mid-cleanup.
func (w *Worker) heartbeatLoop(parentCtx context.Context, job *Job, state *execState, cancelJob context.CancelFunc, done <-chan struct{}, log zerolog.Logger) {
	ticker := time.NewTicker(w.opts.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			hbCtx, cancel := context.WithTimeout(context.WithoutCancel(parentCtx), w.opts.HeartbeatInterval)
			cr, err := w.queue.Heartbeat(hbCtx, job.ID, w.opts.ID)
			cancel()
			switch {
			case errors.Is(err, ErrLostClaim):
				log.Warn().Msg("claim lost (job reaped or finished elsewhere); canceling handler context")
				state.lost.Store(true)
				cancelJob()
				return
			case err != nil:
				// Transient DB trouble: keep trying — if it persists past
				// StaleAfter the reaper takes over and the next heartbeat
				// returns ErrLostClaim.
				log.Warn().Err(err).Msg("heartbeat failed; retrying next tick")
			case cr:
				if !state.cancelRequested.Swap(true) {
					log.Info().Msg("cancel requested; canceling handler context")
					cancelJob()
				}
			}
		}
	}
}

// drainWatch implements graceful drain: when shutdown begins, give the
// handler DrainTimeout to finish, then cancel it (the job gets released).
func (w *Worker) drainWatch(parentCtx context.Context, state *execState, cancelJob context.CancelFunc, done <-chan struct{}, log zerolog.Logger) {
	select {
	case <-done:
		return
	case <-parentCtx.Done():
	}
	timer := time.NewTimer(w.opts.DrainTimeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		log.Warn().Dur("drain_timeout", w.opts.DrainTimeout).
			Msg("drain timeout exceeded; canceling handler context (job will be released)")
		state.drainExpired.Store(true)
		cancelJob()
	}
}

// runHandler dispatches to the registry with panic isolation: a panicking
// handler fails its job (message + stack captured) and never takes the
// executor goroutine down.
func (w *Worker) runHandler(ctx context.Context, job *Job, report ProgressFn) (result any, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("jobs: handler panic: %v\n%s", r, debug.Stack())
		}
	}()
	h := w.registry.Resolve(job.Kind)
	if h == nil {
		// Retried with backoff rather than failed outright: during rolling
		// deploys another worker build may carry the handler; backoff
		// prevents a hot loop and attempts still cap total tries.
		return nil, fmt.Errorf("%w: %q", ErrNoHandler, job.Kind)
	}
	return h.Run(ctx, job, report)
}

// finalize writes the terminal (or requeued) state for a finished handler,
// using a context detached from shutdown so the write lands mid-drain.
func (w *Worker) finalize(parentCtx context.Context, job *Job, state *execState, result any, err error, log zerolog.Logger, elapsed time.Duration) {
	fctx, cancel := context.WithTimeout(context.WithoutCancel(parentCtx), finalizeTimeout)
	defer cancel()

	switch {
	case state.lost.Load():
		// Someone else (reaper/canceler) owns the outcome now.
		log.Warn().Dur("elapsed", elapsed).Msg("job finished after claim loss; no state written")
		return

	case err == nil:
		if _, ferr := w.queue.Succeed(fctx, job.ID, w.opts.ID, result); ferr != nil {
			log.Error().Err(ferr).Msg("recording job success failed (reaper will recover)")
			return
		}
		log.Info().Dur("elapsed", elapsed).Msg("job succeeded")

	case state.cancelRequested.Load():
		// Operator cancel honored (handler stopped with an error after the
		// context was canceled) → terminal canceled.
		if _, ferr := w.queue.MarkCanceled(fctx, job.ID, w.opts.ID, "canceled: "+err.Error()); ferr != nil {
			log.Error().Err(ferr).Msg("recording job cancel failed (reaper will recover)")
			return
		}
		log.Info().Dur("elapsed", elapsed).Msg("job canceled (cooperative)")

	case state.drainExpired.Load() && isContextErr(err):
		// Shutdown interrupted the handler → give the job back, attempt
		// refunded.
		if _, ferr := w.queue.Release(fctx, job.ID, w.opts.ID, "released: worker drain timeout during shutdown"); ferr != nil {
			log.Error().Err(ferr).Msg("releasing job on drain failed (reaper will recover)")
			return
		}
		log.Info().Dur("elapsed", elapsed).Msg("job released back to queue (drain)")

	default:
		failed, ferr := w.queue.Fail(fctx, job.ID, w.opts.ID, err.Error())
		if ferr != nil {
			log.Error().Err(ferr).Msg("recording job failure failed (reaper will recover)")
			return
		}
		log.Warn().Err(err).Dur("elapsed", elapsed).
			Str("outcome", string(failed.Status)).
			Msg("job attempt failed")
	}
}

func isContextErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// jitter randomizes d by ±20% to de-synchronize pollers.
func (w *Worker) jitter(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (0.8 + 0.4*rand.Float64()))
}

// sleep waits d or until ctx is done.
func (w *Worker) sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
