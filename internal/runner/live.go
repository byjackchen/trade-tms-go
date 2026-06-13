package runner

// live.go is the live trading node orchestration (P5 decision 1/2/3/5/6): it
// wires the native moomoo OpenD client (or mock, by TMS_MOOMOO_ADDR) -> a
// streaming MoomooFeed -> a livengine.Session (signal mode, NoopExecutor) ->
// the publish Sink (PG signal_intents append + Redis streams), under the
// ops.commands control plane (halt/resume/kill/stop/set_mode) with full audit.
//
// The node is a SUPERVISOR over a session: it runs one session, and on a
// mode-switch command it restarts the session gracefully (decision 6's
// "mode-switch via graceful session restart"). A halt/kill toggles the
// HaltState the session's EmitGate reads (stop emitting NEW intents; bars keep
// flowing so state stays warm). A stop/kill ends the supervisor.
//
// Lifecycle: ctx-cancellation everywhere, graceful drain, no goroutine leaks,
// structured logs, NO secrets logged (the moomoo creds, if any, never appear).

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"

	"github.com/byjackchen/trade-tms-go/internal/adapters/moomoo"
	"github.com/byjackchen/trade-tms-go/internal/adapters/moomoo/pb/qotcommon"
	"github.com/byjackchen/trade-tms-go/internal/commands"
	"github.com/byjackchen/trade-tms-go/internal/core"
	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/livengine"
	"github.com/byjackchen/trade-tms-go/internal/publish"
)

// LiveConfig configures a live node.
type LiveConfig struct {
	// TraderID is the Redis namespace + sessions.trader_id (required).
	TraderID string
	// Mode is the initial mode (P5: "signal" only).
	Mode string
	// Strategy selects the strategy set ("multi" canonical).
	Strategy string
	// Tickers is the SEPA stock universe (SEPA / multi).
	Tickers []string
	// ORBSymbol is the ORB instrument (orb path).
	ORBSymbol string
	// StartingBalance is the informational health NAV (USD; default 100000).
	StartingBalance float64
	// MoomooAddr is the OpenD endpoint (real or mock; TMS_MOOMOO_ADDR).
	MoomooAddr string
	// MoomooMaxSub caps subscriptions (TMS_MOOMOO_MAX_SUB).
	MoomooMaxSub int
	// BarSeconds is the live K-line width in seconds (86400 daily; 60/300/... intraday).
	BarSeconds int
	// WarmupCalendarDays is the SEPA out-of-band warmup horizon (default 400).
	WarmupCalendarDays int
	// ParamsDir is config.StrategyParamsDir (param override directory).
	ParamsDir string
	// DrainTimeout bounds graceful shutdown.
	DrainTimeout time.Duration
}

// liveUnhealthyAfter is the number of CONSECUTIVE failed session
// assemble/run attempts (without a single successful run in between) after which
// the node reports its inner session unhealthy on /healthz. A node whose session
// cannot stay up — e.g. an empty universe that hard-fails strategy assembly on
// every restart — must NOT present a green liveness probe (finding 2): an
// operator / `compose --wait` gate would otherwise believe the node is emitting
// signals when it is doing nothing. One transient error (reconnect, a single
// assembly hiccup) is tolerated; a persistent crash-loop is surfaced.
const liveUnhealthyAfter = 3

// SessionHealth is the inner trading session's liveness, surfaced on /healthz so
// a crash-looping session is NOT reported as a healthy node (finding 2).
type SessionHealth struct {
	// Running is true once a session has assembled + entered its run loop and has
	// not since ended with an error (a clean stop/restart keeps the last value).
	Running bool `json:"running"`
	// ConsecutiveFailures counts session attempts that ended in error without an
	// intervening successful run. Reset to 0 on a successful session start.
	ConsecutiveFailures int `json:"consecutive_failures"`
	// Healthy is false once ConsecutiveFailures reaches liveUnhealthyAfter (the
	// node is crash-looping and cannot keep a session running).
	Healthy bool `json:"healthy"`
	// LastError is the most recent session-failure message (NO secrets; the
	// assembly/feed errors carry none). Empty when the last attempt succeeded.
	LastError string `json:"last_error,omitempty"`
}

// Live is the live node.
type Live struct {
	cfg       LiveConfig
	pool      *pgxpool.Pool
	rdb       *redis.Client
	assembler *Assembler
	store     *publish.Store
	publisher *publish.Publisher
	halt      *commands.HaltState
	log       zerolog.Logger

	mu   sync.RWMutex
	mode string
	// restart, when set, is the mode the supervisor should restart the session
	// in (a SetMode request). Read + cleared by the supervisor loop.
	restartMode string
	restartCh   chan struct{}

	// session-health fields (finding 2): guarded by mu. The supervisor records a
	// successful session start (markSessionRunning) and a session failure
	// (markSessionFailed); /healthz reads SessionHealth().
	sessionRunning  bool
	sessionFailures int
	sessionLastErr  string
}

// NewLive builds a live node. rdb may be nil (Redis-less: no streams, no command
// notify — the command consumer then polls).
func NewLive(pool *pgxpool.Pool, rdb *redis.Client, cfg LiveConfig, log zerolog.Logger) (*Live, error) {
	if cfg.TraderID == "" {
		return nil, errors.New("runner: live node requires a trader id")
	}
	mode := cfg.Mode
	if mode == "" {
		mode = string(livengine.ModeSignal)
	}
	if mode != string(livengine.ModeSignal) {
		return nil, fmt.Errorf("runner: live mode %q not wired in P5 (signal only; paper/live deferred to P6)", mode)
	}
	if cfg.BarSeconds <= 0 {
		cfg.BarSeconds = 86400 // daily
	}
	if cfg.DrainTimeout <= 0 {
		cfg.DrainTimeout = 10 * time.Second
	}
	pub := publish.NewPublisher(rdb, publish.Options{TraderID: cfg.TraderID, Logger: log})
	return &Live{
		cfg:       cfg,
		pool:      pool,
		rdb:       rdb,
		assembler: NewAssembler(pool, cfg.ParamsDir),
		store:     publish.NewStore(pool),
		publisher: pub,
		halt:      commands.NewHaltState(time.Now),
		log:       log.With().Str("component", "live-node").Str("trader_id", cfg.TraderID).Logger(),
		mode:      mode,
		restartCh: make(chan struct{}, 1),
	}, nil
}

// HaltState exposes the node's halt state (for the runner's /healthz + session).
func (l *Live) HaltState() *commands.HaltState { return l.halt }

// SessionHealth returns the inner session's liveness for /healthz (finding 2).
func (l *Live) SessionHealth() SessionHealth {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return SessionHealth{
		Running:             l.sessionRunning,
		ConsecutiveFailures: l.sessionFailures,
		Healthy:             l.sessionFailures < liveUnhealthyAfter,
		LastError:           l.sessionLastErr,
	}
}

// markSessionRunning records a session that successfully assembled and entered
// its run loop: clears the consecutive-failure counter + last error.
func (l *Live) markSessionRunning() {
	l.mu.Lock()
	l.sessionRunning = true
	l.sessionFailures = 0
	l.sessionLastErr = ""
	l.mu.Unlock()
}

// markSessionFailed records a session that ended with a (non-shutdown) error:
// the node is not running a session and the failure counter advances toward the
// unhealthy threshold.
func (l *Live) markSessionFailed(err error) {
	l.mu.Lock()
	l.sessionRunning = false
	l.sessionFailures++
	if err != nil {
		l.sessionLastErr = err.Error()
	}
	l.mu.Unlock()
}

// Mode returns the current mode (commands.Controller).
func (l *Live) Mode() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.mode
}

// SetMode requests a mode switch (commands.Controller). P5 accepts only
// "signal"; paper/live are rejected (deferred to P6). A successful switch to the
// SAME mode is a no-op; a switch is applied via graceful session restart.
func (l *Live) SetMode(_ context.Context, mode string) error {
	if mode != string(livengine.ModeSignal) {
		return fmt.Errorf("mode %q not wired in P5 (signal only; paper/live deferred to P6)", mode)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.mode == mode && l.restartMode == "" {
		return nil // already in the requested mode
	}
	l.restartMode = mode
	select {
	case l.restartCh <- struct{}{}:
	default:
	}
	return nil
}

// Halt stops emitting new intents + records a tms.halts row (commands.Controller).
func (l *Live) Halt(ctx context.Context, kind commands.HaltKind, reason string) error {
	l.halt.Halt(kind, reason)
	l.recordHalt(ctx, kind, reason)
	l.log.Warn().Str("kind", string(kind)).Str("reason", reason).Msg("live node halted (emitting paused)")
	return nil
}

// Resume clears a manual halt + clears the active tms.halts row
// (commands.Controller).
func (l *Live) Resume(ctx context.Context) error {
	l.halt.Resume()
	l.clearHalt(ctx, "command")
	l.log.Info().Msg("live node resumed (emitting)")
	return nil
}

// Stop requests a graceful node shutdown (commands.Controller).
func (l *Live) Stop(_ context.Context, reason string) error {
	l.halt.Stop()
	select {
	case l.restartCh <- struct{}{}:
	default:
	}
	l.log.Info().Str("reason", reason).Msg("live node stop requested")
	return nil
}

// Kill is the kill switch: halt + stop (commands.Controller).
func (l *Live) Kill(ctx context.Context, reason string) error {
	l.halt.Halt(commands.HaltManual, reason)
	l.recordHalt(ctx, commands.HaltManual, reason)
	l.halt.Stop()
	select {
	case l.restartCh <- struct{}{}:
	default:
	}
	l.log.Warn().Str("reason", reason).Msg("live node KILLED (halt + stop)")
	return nil
}

// Run is the supervisor: it starts the moomoo client + command consumer, then
// runs sessions (restarting on a mode-switch) until ctx is cancelled or a
// stop/kill is requested. It returns nil on a clean shutdown.
func (l *Live) Run(ctx context.Context) error {
	// (1) moomoo client (real or mock by addr). The push handler is installed on
	// the feed per session; here we build the client once and reuse it across
	// restarts.
	klType, err := moomoo.KLTypeForSeconds(l.cfg.BarSeconds)
	if err != nil {
		return fmt.Errorf("runner: %w", err)
	}

	// A shared feed forwards pushes; its PushHandler is the client's OnKLine. The
	// universe is resolved at first assembly; we (re)subscribe per session.
	feed := NewMoomooFeed(nil, klType, 0, l.log) // symbols set per session

	client := moomoo.NewClient(moomoo.Options{
		Addr:             l.cfg.MoomooAddr,
		MaxSubscriptions: l.cfg.MoomooMaxSub,
		Logger:           l.log,
		OnKLine:          feed.PushHandler,
	})
	client.Start(ctx)
	defer func() { _ = client.Close() }()

	readyCtx, cancelReady := context.WithTimeout(ctx, 30*time.Second)
	if err := client.Ready(readyCtx); err != nil {
		cancelReady()
		return fmt.Errorf("runner: moomoo not ready: %w", err)
	}
	cancelReady()
	l.log.Info().Str("addr", l.cfg.MoomooAddr).Msg("moomoo client connected")

	// (2) Command consumer (ops.commands -> Controller). Runs for the node's life.
	consumer, err := commands.NewConsumer(commands.ConsumerOptions{
		Pool:       l.pool,
		Redis:      l.rdb,
		Controller: l,
		Actor:      "tms-live:" + l.cfg.TraderID,
		Logger:     l.log,
	})
	if err != nil {
		return err
	}
	var consumerWG sync.WaitGroup
	consumerWG.Add(1)
	go func() {
		defer consumerWG.Done()
		_ = consumer.Run(ctx)
	}()
	defer consumerWG.Wait()

	// (3) Open a session row in tms.sessions (one running per trader).
	sessionID, err := l.openSession(ctx)
	if err != nil {
		return err
	}
	defer l.closeSession(context.WithoutCancel(ctx), sessionID, "STOPPED")

	// (4) Supervisor loop: run a session; restart on a mode-switch; exit on stop.
	for {
		if ctx.Err() != nil {
			return nil
		}
		if l.halt.IsStopped() {
			l.log.Info().Msg("live node stopped (supervisor exiting)")
			return nil
		}
		mode := l.takeRestartMode()
		runErr := l.runSession(ctx, client, feed, klType, sessionID, mode)
		if runErr != nil && ctx.Err() == nil && !l.halt.IsStopped() {
			// A session error that is not a shutdown: advance the failure counter
			// (so /healthz goes unhealthy once the node is crash-looping — finding
			// 2), log + back off before restarting (the client auto-reconnects; the
			// session just re-opens).
			l.markSessionFailed(runErr)
			health := l.SessionHealth()
			ev := l.log.Error().Err(runErr).Int("consecutive_failures", health.ConsecutiveFailures)
			if !health.Healthy {
				ev = ev.Bool("healthz_unhealthy", true)
			}
			ev.Msg("live session ended with error; restarting after backoff")
			select {
			case <-time.After(2 * time.Second):
			case <-ctx.Done():
				return nil
			}
		}
	}
}

// runSession assembles + runs ONE session until ctx cancellation, a stop/kill,
// or a restart request. Returns nil on a clean stop/restart.
func (l *Live) runSession(ctx context.Context, client MoomooClient, feed *MoomooFeed, klType qotcommon.KLType, sessionID int64, mode string) error {
	// Today's window for the live session: the warmup horizon up to "now".
	now := time.Now().UTC()
	asOf := calendar.NewDate(now.Year(), now.Month(), now.Day())
	windowDays := l.cfg.WarmupCalendarDays
	if windowDays <= 0 {
		windowDays = DefaultEODWarmupCalendarDays
	}
	windowStart := asOf.AddDays(-windowDays)

	as, err := l.assembler.Assemble(ctx, AssemblyInput{
		Strategy:        l.cfg.Strategy,
		Tickers:         l.cfg.Tickers,
		ORBSymbol:       l.cfg.ORBSymbol,
		StartingBalance: l.startingBalance(),
	}, windowStart, asOf)
	if err != nil {
		return err
	}

	// Subscribe the live universe + set the feed symbols for this session.
	feed.symbols = as.Tickers
	if err := feed.Subscribe(ctx, client); err != nil {
		return err
	}

	// Warmup provider over moomoo history (the SEPA out-of-band tail).
	warmupBegin := time.Date(windowStart.Year, windowStart.Month, windowStart.Day, 0, 0, 0, 0, time.UTC)
	warmupEnd := time.Date(asOf.Year, asOf.Month, asOf.Day, 0, 0, 0, 0, time.UTC)
	warmup := NewMoomooWarmup(client, klType, warmupBegin, warmupEnd)

	startMoney, err := domain.MoneyFromFloat64(l.startingBalance())
	if err != nil {
		return fmt.Errorf("runner: invalid starting balance: %w", err)
	}

	sink := NewSink(SinkOptions{
		Store:     l.store,
		Publisher: l.publisher,
		Mode:      SinkAppend,
		SessionID: &sessionID,
		Logger:    l.log,
	})

	sess, err := livengine.NewSession(livengine.Config{
		Mode:            livengine.ModeSignal,
		Strategies:      as.Assembly.Strategies,
		Portfolio:       as.Assembly.Portfolio,
		Context:         as.Assembly.Context,
		SPYSymbol:       as.SPYSymbol,
		Warmup:          warmup,
		WarmupSymbols:   as.WarmupSymbols,
		StartingBalance: startMoney,
		Sink:            sink,
		EmitGate:        l.halt.Emitting, // halt = stop emitting NEW intents
	})
	if err != nil {
		return fmt.Errorf("runner: building live session: %w", err)
	}

	// Prime warmup from moomoo history (out-of-band, before the loop).
	if err := sess.Prime(ctx); err != nil {
		return fmt.Errorf("runner: priming warmup: %w", err)
	}

	// Publish the watchlist for cockpit continuity (best-effort).
	if l.publisher != nil {
		_ = l.publisher.PublishWatchlist(ctx, as.Tickers, warmupEnd.UnixNano())
	}

	l.setRunningMode(mode)
	// The session assembled + primed + subscribed and is about to enter its run
	// loop: mark it healthy (clears any prior crash-loop failure count — finding 2).
	l.markSessionRunning()
	l.log.Info().Str("mode", mode).Int("instruments", len(as.Tickers)).
		Int("warmup_symbols", len(as.WarmupSymbols)).Msg("live session running (signal mode)")

	// runCtx is cancelled when ctx is cancelled OR a restart/stop is requested.
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	var watchWG sync.WaitGroup
	watchWG.Add(1)
	go func() {
		defer watchWG.Done()
		select {
		case <-ctx.Done():
		case <-l.restartCh:
		}
		cancelRun()
	}()

	// Run the streaming session over the wall clock + moomoo feed.
	err = sess.RunStream(runCtx, feed, core.StreamWall, nil)
	cancelRun()
	watchWG.Wait()

	// A cancelled runCtx with the parent still live is a clean restart/stop.
	if errors.Is(err, context.Canceled) && ctx.Err() == nil {
		return nil
	}
	return err
}

// takeRestartMode returns the pending restart mode (or the current mode) and
// clears the pending flag.
func (l *Live) takeRestartMode() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.restartMode != "" {
		l.mode = l.restartMode
		l.restartMode = ""
	}
	return l.mode
}

func (l *Live) setRunningMode(mode string) {
	l.mu.Lock()
	l.mode = mode
	l.mu.Unlock()
}

func (l *Live) startingBalance() float64 {
	if l.cfg.StartingBalance > 0 {
		return l.cfg.StartingBalance
	}
	return 100000
}

// ---------------------------------------------------------------------------
// tms.sessions + tms.halts persistence
// ---------------------------------------------------------------------------

// openSession inserts a RUNNING session row (one per trader id by the partial
// unique index). A pre-existing running row is reused (idempotent restart).
func (l *Live) openSession(ctx context.Context) (int64, error) {
	var id int64
	err := l.pool.QueryRow(ctx,
		`INSERT INTO tms.sessions (trader_id, mode, status)
		 VALUES ($1, $2, 'RUNNING') RETURNING id`,
		l.cfg.TraderID, l.Mode()).Scan(&id)
	if err == nil {
		l.log.Info().Int64("session_id", id).Msg("opened live session row")
		return id, nil
	}
	// A running session already exists (unique index): reuse it.
	rerr := l.pool.QueryRow(ctx,
		`SELECT id FROM tms.sessions WHERE trader_id=$1 AND status='RUNNING' ORDER BY started_at DESC LIMIT 1`,
		l.cfg.TraderID).Scan(&id)
	if rerr != nil {
		return 0, fmt.Errorf("runner: opening session: %w (and no reusable running row: %v)", err, rerr)
	}
	l.log.Info().Int64("session_id", id).Msg("reusing existing running session row")
	return id, nil
}

// closeSession marks a session ended (best-effort; on shutdown).
func (l *Live) closeSession(ctx context.Context, id int64, status string) {
	if _, err := l.pool.Exec(ctx,
		`UPDATE tms.sessions SET status=$2, ended_at=now() WHERE id=$1 AND status='RUNNING'`,
		id, status); err != nil {
		l.log.Warn().Err(err).Int64("session_id", id).Msg("closing session row failed")
	}
}

// recordHalt inserts an active tms.halts row (best-effort audit).
func (l *Live) recordHalt(ctx context.Context, kind commands.HaltKind, reason string) {
	if reason == "" {
		reason = string(kind)
	}
	if _, err := l.pool.Exec(ctx,
		`INSERT INTO tms.halts (kind, reason) VALUES ($1, $2)`,
		string(kind), reason); err != nil {
		l.log.Warn().Err(err).Msg("recording halt row failed")
	}
}

// clearHalt clears any active tms.halts rows (best-effort).
func (l *Live) clearHalt(ctx context.Context, by string) {
	if _, err := l.pool.Exec(ctx,
		`UPDATE tms.halts SET cleared_at=now(), cleared_by=$1 WHERE cleared_at IS NULL`,
		by); err != nil {
		l.log.Warn().Err(err).Msg("clearing halt rows failed")
	}
}

// compile-time check: *Live is a commands.Controller.
var _ commands.Controller = (*Live)(nil)
