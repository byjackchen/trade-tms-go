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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"

	"github.com/byjackchen/trade-tms-go/internal/accounting"
	"github.com/byjackchen/trade-tms-go/internal/adapters/moomoo"
	"github.com/byjackchen/trade-tms-go/internal/adapters/moomoo/pb/qotcommon"
	"github.com/byjackchen/trade-tms-go/internal/commands"
	"github.com/byjackchen/trade-tms-go/internal/core"
	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/data/universe"
	"github.com/byjackchen/trade-tms-go/internal/domain"
	moexec "github.com/byjackchen/trade-tms-go/internal/exec/moomoo"
	"github.com/byjackchen/trade-tms-go/internal/livengine"
	"github.com/byjackchen/trade-tms-go/internal/livetrade"
	"github.com/byjackchen/trade-tms-go/internal/riskgate"
	"github.com/byjackchen/trade-tms-go/internal/publish"
)

// The live node's runtime control-plane vocabulary: the operator switches a
// running node between these via the set_mode command (decision 6's "mode-switch
// via graceful session restart"). These are NOT a domain type — they map onto the
// 2D model (docs/concept-alignment.md §1.3): each value derives an ExecutionPolicy
// (modeSignal → ExecSignal; modePaper/modeLive → ExecAuto) and a bound Account env
// (signal → sim, paper → simulate, live → real) via resolveAccount/execPolicyFor.
const (
	modeSignal = "signal"
	modePaper  = "paper"
	modeLive   = "live"
)

// execPolicyForMode maps a control-plane mode onto the execution axis: signal
// emits only; paper/live auto-submit against the bound account.
func execPolicyForMode(mode string) domain.ExecutionPolicy {
	if mode == modeSignal {
		return domain.ExecSignal
	}
	return domain.ExecAuto
}

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

	// --- paper/live trading config (P6) ---

	// PaperAccID is the moomoo SIMULATE account id (required for paper mode).
	PaperAccID uint64
	// LiveAccID is the moomoo REAL account id (required for live mode; never
	// defaulted — there is no path to real money without an explicit acc id).
	LiveAccID uint64
	// UnlockPassword unlocks the REAL account (live mode only; from a secret env).
	UnlockPassword string
	// LiveConfirmationPhrase must equal moexec.LiveConfirmationPhrase to activate
	// live mode (typed confirmation, decision 8). Empty => live activation refused.
	LiveConfirmationPhrase string
	// ReconcileInterval is the periodic reconciliation cadence (paper/live;
	// default 5m). On-demand reconciliation is always available via the command.
	ReconcileInterval time.Duration
	// ReconcileTolerance absorbs tiny position diffs (shares; default 0 = exact).
	ReconcileTolerance int64
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

	// active trade-session handle (paper/live), guarded by mu. The flatten /
	// emergency-kill / reconcile commands operate on it. nil in signal mode or
	// between sessions.
	tradeSession *livetrade.TradeSession
	reconciler   *livetrade.Reconciler

	// sessionID is the open tms.sessions row id (set once by Run after openSession,
	// reused across session restarts). Halt rows are scoped to it so the active
	// halt for THIS trader can be rehydrated on restart, and a stale halt from a
	// prior crashed session is never re-applied. Guarded by mu; 0 before openSession.
	sessionID int64

	// client is the shared moomoo trading/market client, set by Run once the OpenD
	// connection is ready. The MANUAL desk borrows its TradeClient to bind its own
	// executor (a manual session can be established while the strategy session stays
	// in signal mode). nil before Run connects. Guarded by mu.
	client MoomooClient

	// manual is the operator-driven manual trade desk (the discretionary order
	// surface), independent of the strategy execution mode. nil until
	// ConnectManualSession binds it. Guarded by manualMu so a connect + an in-flight
	// place never race. Its executor holds an INDEPENDENT accounting book attributed
	// to the MANUAL pseudo-strategy so manual fills never mingle with the auto books.
	manualMu sync.Mutex
	manual   *livetrade.ManualController
}

// NewLive builds a live node. rdb may be nil (Redis-less: no streams, no command
// notify — the command consumer then polls).
func NewLive(pool *pgxpool.Pool, rdb *redis.Client, cfg LiveConfig, log zerolog.Logger) (*Live, error) {
	if cfg.TraderID == "" {
		return nil, errors.New("runner: live node requires a trader id")
	}
	mode := cfg.Mode
	if mode == "" {
		mode = modeSignal
	}
	switch mode {
	case modeSignal:
	case modePaper:
		if cfg.PaperAccID == 0 {
			return nil, fmt.Errorf("runner: paper mode requires a SIMULATE acc id (TMS_MOOMOO_PAPER_ACC_ID)")
		}
	case modeLive:
		// SAFETY (decision 8): live mode needs the real acc id + the typed
		// confirmation phrase configured up front. The MoomooExecutor re-asserts
		// the full gate (phrase + acc id + UnlockTrade + trader-id) at activation.
		if cfg.LiveAccID == 0 {
			return nil, fmt.Errorf("runner: live mode requires a REAL acc id (TMS_MOOMOO_LIVE_ACC_ID) — refusing to activate")
		}
		if cfg.LiveConfirmationPhrase != moexec.LiveConfirmationPhrase {
			return nil, fmt.Errorf("runner: live mode requires the exact confirmation phrase (TMS_LIVE_CONFIRM) — refusing to activate")
		}
		if cfg.TraderID != moexec.LiveTraderID {
			return nil, fmt.Errorf("runner: live mode requires trader-id %q (the distinct real-money namespace)", moexec.LiveTraderID)
		}
	default:
		return nil, fmt.Errorf("runner: unknown live mode %q (want signal|paper|live)", mode)
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

// SetMode requests a mode switch (commands.Controller). signal/paper/live are
// all valid; a switch to paper/live requires the corresponding broker creds to
// have been configured at node start (the confirmation gate is enforced at the
// API boundary + the MoomooExecutor activation). A switch to the SAME mode is a
// no-op; a switch is applied via a graceful session restart.
//
// SAFETY: switching TO live requires the live acc id + confirmation phrase to
// have been configured (NewLive enforced them). A node started without live
// creds can never switch to live at runtime — there is no code path to real
// money without the up-front gate.
func (l *Live) SetMode(_ context.Context, mode string) error {
	switch mode {
	case modeSignal:
	case modePaper:
		if l.cfg.PaperAccID == 0 {
			return fmt.Errorf("cannot switch to paper: no SIMULATE acc id configured")
		}
	case modeLive:
		if l.cfg.LiveAccID == 0 || l.cfg.LiveConfirmationPhrase != moexec.LiveConfirmationPhrase {
			return fmt.Errorf("cannot switch to live: real acc id + confirmation phrase not configured (refusing real money)")
		}
		if l.cfg.TraderID != moexec.LiveTraderID {
			return fmt.Errorf("cannot switch to live: trader-id must be the %q namespace", moexec.LiveTraderID)
		}
	default:
		return fmt.Errorf("unknown mode %q (want signal|paper|live)", mode)
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

// Flatten closes all open positions in the active paper/live session (decision
// 7). signal mode has no positions, so it is a no-op error. The flatten is
// idempotent + audited (the executor tracks each closing order).
func (l *Live) Flatten(ctx context.Context, reason string) (int, error) {
	ts := l.activeTradeSession()
	if ts == nil {
		return 0, fmt.Errorf("flatten: no active paper/live trading session (signal mode has no positions)")
	}
	coids, err := ts.Flatten(ctx, livetrade.FlattenConfirmationPhrase, reason)
	if err != nil {
		return 0, err
	}
	l.log.Warn().Int("orders", len(coids)).Str("reason", reason).Msg("FLATTEN: closing all positions")
	return len(coids), nil
}

// EmergencyKill is the panic button (decision 5): halt + flatten + stop, in that
// order. It halts first (suppress NEW opens), flattens all positions, records a
// halt row, then stops the node. Returns the count of closing orders submitted.
func (l *Live) EmergencyKill(ctx context.Context, reason string) (int, error) {
	if reason == "" {
		reason = "emergency kill"
	}
	// 1. Halt (suppress new opens immediately).
	l.halt.Halt(commands.HaltManual, reason)
	l.recordHalt(ctx, commands.HaltManual, reason)
	// 2. Flatten (close everything). A signal-mode node has nothing to flatten.
	n := 0
	if ts := l.activeTradeSession(); ts != nil {
		coids, ferr := ts.Flatten(ctx, livetrade.FlattenConfirmationPhrase, "emergency-kill: "+reason)
		if ferr != nil {
			l.log.Error().Err(ferr).Msg("emergency-kill: flatten failed (continuing to stop)")
		}
		n = len(coids)
	}
	// 3. Stop the node (hard).
	l.halt.Stop()
	select {
	case l.restartCh <- struct{}{}:
	default:
	}
	l.log.Warn().Str("reason", reason).Int("flattened_orders", n).Msg("EMERGENCY KILL (halt + flatten + stop)")
	return n, nil
}

// activeTradeSession returns the running paper/live trade session (nil in signal
// mode / between sessions).
func (l *Live) activeTradeSession() *livetrade.TradeSession {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.tradeSession
}

// setTradeSession records the active paper/live trade session + reconciler (or
// clears them when a session ends).
func (l *Live) setTradeSession(ts *livetrade.TradeSession, rec *livetrade.Reconciler) {
	l.mu.Lock()
	l.tradeSession = ts
	l.reconciler = rec
	l.mu.Unlock()
}

// Reconcile runs an on-demand reconciliation against the active paper/live
// session (commands.Controller; the 'tms reconcile' command). It persists the
// report (-> tms.reconciliation_reports, read by the endpoint) and alerts on a
// mismatch (halt + cockpit, NO auto-correct). Returns whether drift was found.
func (l *Live) Reconcile(ctx context.Context) (bool, error) {
	l.mu.RLock()
	rec := l.reconciler
	l.mu.RUnlock()
	if rec == nil {
		return false, fmt.Errorf("reconcile: no active paper/live trading session (signal mode has no broker positions)")
	}
	r, err := rec.Reconcile(ctx)
	if err != nil {
		return false, err
	}
	if r.HasIssues() {
		l.log.Warn().Str("summary", r.Summary()).Msg("on-demand reconciliation found drift")
	} else {
		l.log.Info().Int("matched", len(r.Matched)).Msg("on-demand reconciliation clean")
	}
	return r.HasIssues(), nil
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
	l.mu.Lock()
	l.client = client
	l.mu.Unlock()
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

	// (3) Open a session row in tms.sessions (one running per trader). The id is
	// stored on the node so halt rows can be scoped to it (record/clear/rehydrate).
	sessionID, err := l.openSession(ctx)
	if err != nil {
		return err
	}
	l.mu.Lock()
	l.sessionID = sessionID
	l.mu.Unlock()
	defer l.closeSession(context.WithoutCancel(ctx), sessionID, "STOPPED")

	// (3a) DATABASE-ORIENTED restart continuity: rehydrate a latched halt from the
	// durable tms.halts row BEFORE the supervisor runs the first session, so a
	// crash/restart cannot silently clear an operator/operational halt and resume
	// emitting or trading. Without this the fresh HaltState (NewHaltState) would
	// report "running" and the node would re-arm a halted trader.
	l.rehydrateHalt(ctx)

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

	// LIVE-ONLY subscription cap. The assembly sizes the SEPA universe to fit the
	// moomoo OpenD per-connection quota (subscriptionCap, default 100) via the
	// SHARED universe.ResolveLiveSubscriptionSet, with the env top-N
	// (TMS_LIVE_UNIVERSE_LIMIT, default 85) as an additional clamp. A 0 cap would
	// mean "no cap" (the full survivor-bias-free set, 4000+ names) which the OpenD
	// subscribe rejects — so live always passes the positive quota here.
	// backtest / hyperopt / EOD pass 0 and keep the full universe.
	subscriptionCap := l.subscriptionCap()
	envLimit, err := universe.ResolveUniverseLimit(nil)
	if err != nil {
		return fmt.Errorf("runner: %w", err)
	}

	as, err := l.assembler.Assemble(ctx, AssemblyInput{
		Strategy:        l.cfg.Strategy,
		Tickers:         l.cfg.Tickers,
		ORBSymbol:       l.cfg.ORBSymbol,
		StartingBalance: l.startingBalance(),
		SubscriptionCap: subscriptionCap,
		UniverseLimit:   envLimit,
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

	// Multi-symbol BatchWarmupConsumer strategies (sector / pairs) prime from the
	// INTERLEAVED pre-window history of their instruments. Unlike the EOD replay —
	// which builds this state in-band from the replayed window — the LIVE feed only
	// delivers bars from "now" forward, so without this the sector momentum ranking
	// / pairs spread would start COLD (all no_setup) until ~lookback live bars
	// accumulate (months). Build the interleaved stream over the moomoo warmup
	// history (strictly before warmupEnd / "now"), so a freshly-started live
	// sector/pairs session emits actionable states from the first bar.
	warmupBatch, err := l.assembler.BuildWarmupBatch(ctx, as, warmup, warmupEnd)
	if err != nil {
		return fmt.Errorf("runner: building batch warmup: %w", err)
	}

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

	// Build the runnable session by mode: signal -> livengine.Session (NoopExecutor),
	// paper/live -> livetrade.TradeSession (gated MoomooExecutor). Both share the
	// SAME assembled strategies / gate / context / warmup.
	stream, primeFn, cleanup, err := l.buildRunnable(ctx, mode, as, warmup, warmupBatch, startMoney, sink, sessionID, client, warmupEnd)
	if err != nil {
		return err
	}
	defer cleanup()

	// Prime warmup (signal) / restore-state + warmup (paper/live), out of band.
	if err := primeFn(ctx); err != nil {
		return fmt.Errorf("runner: priming session: %w", err)
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
		Int("warmup_symbols", len(as.WarmupSymbols)).
		Int("batch_warmup_symbols", len(as.WarmupBatchSymbols)).
		Int("batch_warmup_bars", len(warmupBatch)).Msg("live session running")

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
	err = stream.RunStream(runCtx, feed, core.StreamWall, nil)
	cancelRun()
	watchWG.Wait()

	// A cancelled runCtx with the parent still live is a clean restart/stop.
	if errors.Is(err, context.Canceled) && ctx.Err() == nil {
		return nil
	}
	return err
}

// streamRunner is the minimal run surface shared by the signal Session and the
// paper/live TradeSession's underlying session.
type streamRunner interface {
	RunStream(ctx context.Context, feed livengine.StreamFeed, mode core.StreamClockMode, vc *core.VirtualClock) error
}

// buildRunnable constructs the mode-appropriate runnable: for signal a plain
// livengine.Session; for paper/live a fully-wired livetrade.TradeSession (account
// + gated MoomooExecutor + reconciler + PG persistence) over the SAME assembled
// strategies. It returns the stream runner, the prime function, and a cleanup
// that detaches the active trade session.
func (l *Live) buildRunnable(ctx context.Context, mode string, as *Assembled, warmup livengine.WarmupProvider, warmupBatch []domain.Bar, startMoney domain.Money, sink *Sink, sessionID int64, client MoomooClient, warmupEnd time.Time) (streamRunner, func(context.Context) error, func(), error) {
	if execPolicyForMode(mode) == domain.ExecSignal {
		// Keep the reused sessions row's account_id consistent with the account this
		// (re)built signal session binds. A node can START in paper/live and switch
		// BACK to signal at runtime; without this the session row would keep the
		// stale paper/live account_id while the signal session emits no broker rows.
		if err := l.syncSessionAccount(ctx, sessionID, l.resolveAccount(mode)); err != nil {
			return nil, nil, nil, err
		}
		sess, err := livengine.NewSession(livengine.Config{
			Exec:            domain.ExecSignal,
			Strategies:      as.Assembly.Strategies,
			Gate:       as.Assembly.Gate,
			Context:         as.Assembly.Context,
			SPYSymbol:       as.SPYSymbol,
			Warmup:          warmup,
			WarmupSymbols:   as.WarmupSymbols,
			WarmupBatch:     warmupBatch,
			StartingBalance: startMoney,
			Sink:            sink,
			EmitGate:        l.halt.Emitting,
		})
		if err != nil {
			return nil, nil, nil, fmt.Errorf("runner: building signal session: %w", err)
		}
		return sess, sess.Prime, func() {}, nil
	}

	// Paper/live (ExecAuto): wire the trading stack against the resolved Account.
	// The Account's Env (simulate/real) carries the paper-vs-live axis end to end —
	// the executor derives TrdEnv + the live gate from it, persistence stamps its id.
	acct := l.resolveAccount(mode)

	// Ensure the account row exists (FK target) BEFORE any session/order/position
	// write references it.
	persist := NewLivePersist(l.pool, l.publisher, sessionID, acct.ID, l.cfg.TraderID, "MOOMOO", l.log)
	if err := persist.UpsertAccount(ctx, acct); err != nil {
		return nil, nil, nil, fmt.Errorf("runner: ensuring account row: %w", err)
	}
	// Re-stamp the reused sessions row's account_id to the account this session's
	// orders/positions/fills/reconciliation rows are now attributed to. The session
	// row is opened ONCE (openSession) from the node's initial mode and reused across
	// mode-switch restarts; on an account-changing switch (signal->paper, signal->live,
	// paper->live) this keeps sessions.account_id in lockstep with LivePersist's
	// acct.ID so the one-account-per-(session_id) invariant the position key
	// (account_id, strategy_id, symbol) + blotter indexes rely on holds. The account
	// row already exists (UpsertAccount above) so the FK is satisfiable.
	if err := l.syncSessionAccount(ctx, sessionID, acct); err != nil {
		return nil, nil, nil, err
	}

	book := accounting.NewAccount(startMoney, nil)
	account := livetrade.NewAccountAdapter(book)

	execCfg := moexec.Config{
		Account:  acct,
		Client:   client.TradeClient(),
		TraderID: l.cfg.TraderID,
		// The executor settles fills into Account + persists + publishes them; the
		// Sink only needs to be a non-nil terminal (the engine equity feed is the
		// account itself in paper/live).
		Sink:     noopFillSink{},
		Book:     account,
		Persist:  persist,
		Risk:     persist,
		Strategy: persist, // re-key restored in-flight orders to their strategy (recovery)
		Logf:     func(f string, a ...any) { l.log.Warn().Msgf("exec: "+f, a...) },
	}
	if acct.IsReal() {
		execCfg.ConfirmationPhrase = l.cfg.LiveConfirmationPhrase
		execCfg.UnlockPassword = l.cfg.UnlockPassword
	}
	exec, err := moexec.New(ctx, execCfg)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("runner: activating %s executor: %w", mode, err)
	}

	ts, err := livetrade.NewTradeSession(livetrade.TradeSessionConfig{
		Acct:          acct,
		Strategies:    as.Assembly.Strategies,
		Gate:          as.Assembly.Gate,
		Context:       as.Assembly.Context,
		SPYSymbol:     as.SPYSymbol,
		Warmup:        warmup,
		WarmupSymbols: as.WarmupSymbols,
		WarmupBatch:   warmupBatch,
		Account:       account,
		Executor:      exec,
		Halt:          l.halt,
		Risk:          persist,
		NAV:           startMoney,
		IntentSink:    sink,
		EmitGate:      l.halt.Emitting,
		StateStore:    persist,
		HealthSink:    persist,
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("runner: building %s trade session: %w", mode, err)
	}

	// Build the reconciler (periodic + on-demand for the flatten/reconcile cmds).
	rec, err := livetrade.NewReconciler(livetrade.ReconcilerConfig{
		Broker:          client.TradeClient(),
		Books:           account,
		Sink:            persist,
		Alerter:         l.reconcileAlerter(),
		AccID:           acct.BrokerAccID,
		Env:             execEnv(acct),
		ToleranceShares: l.cfg.ReconcileTolerance,
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("runner: building reconciler: %w", err)
	}

	// CRASH RECOVERY (decision 6): restore broker positions + run a reconcile
	// before the loop. The strategy SG state is restored inside ts.Prime.
	//
	// RestoreFromBroker returns the broker positions AND (separately) any
	// per-strategy attribution gap on a restored in-flight order. A nil positions
	// slice means a hard restore failure (continue cold); a non-nil slice with a
	// non-nil error means positions were seeded but at least one in-flight order
	// could not be re-keyed to its strategy — we surface that loudly (a restored
	// fill on such an order would be rejected by Fill.Validate, not mis-attributed)
	// yet STILL reconcile, since the netted position book is authoritative.
	restoredPos, rerr := ts.RestoreFromBroker(ctx)
	if restoredPos == nil && rerr != nil {
		l.log.Warn().Err(rerr).Msg("recovery: restore from broker failed (continuing cold)")
	} else {
		if rerr != nil {
			l.log.Error().Err(rerr).Msg("recovery: in-flight order strategy attribution gap " +
				"(positions seeded; affected fills will be rejected until reconciled)")
		}
		if _, recErr := rec.Reconcile(ctx); recErr != nil {
			l.log.Warn().Err(recErr).Msg("recovery: initial reconciliation failed")
		}
	}

	l.setTradeSession(ts, rec)

	// Periodic reconciliation in the background.
	reconcileEvery := l.cfg.ReconcileInterval
	if reconcileEvery <= 0 {
		reconcileEvery = 5 * time.Minute
	}
	reconcileCtx, cancelReconcile := context.WithCancel(ctx)
	var reconcileWG sync.WaitGroup
	reconcileWG.Add(1)
	go func() {
		defer reconcileWG.Done()
		rec.RunPeriodic(reconcileCtx, reconcileEvery, func(e error) {
			l.log.Warn().Err(e).Msg("periodic reconciliation failed")
		})
	}()

	cleanup := func() {
		cancelReconcile()
		reconcileWG.Wait()
		l.setTradeSession(nil, nil)
	}
	return ts.Session(), ts.Prime, cleanup, nil
}

// reconcileAlerter halts the node on a reconciliation mismatch + surfaces it to
// the cockpit (NO auto-correct, decision 5).
func (l *Live) reconcileAlerter() livetrade.MismatchAlerter {
	return reconcileAlerterFunc(func(ctx context.Context, r riskgate.ReconciliationReport) {
		l.halt.Halt(commands.HaltReconciliation, "reconciliation mismatch detected (no auto-correct)")
		l.recordHalt(ctx, commands.HaltReconciliation, "reconciliation mismatch: "+r.Summary())
		l.log.Error().Str("summary", r.Summary()).Msg("RECONCILIATION MISMATCH — halted; human resolution required")
	})
}

// reconcileAlerterFunc adapts a func to livetrade.MismatchAlerter.
type reconcileAlerterFunc func(context.Context, riskgate.ReconciliationReport)

func (f reconcileAlerterFunc) OnReconciliationMismatch(ctx context.Context, r riskgate.ReconciliationReport) {
	f(ctx, r)
}

// noopFillSink is the terminal executor fill sink for paper/live (accounting +
// persistence + publish are done by the executor's effect handler; the sink is
// the engine-feed seam, unused here).
type noopFillSink struct{}

func (noopFillSink) EmitFill(domain.Fill) error { return nil }

// execEnv maps an account to the broker TrdEnv (simulate -> SIMULATE, real -> REAL).
func execEnv(acct domain.Account) moomoo.TrdEnv {
	if acct.IsReal() {
		return moomoo.TrdEnvReal
	}
	return moomoo.TrdEnvSimulate
}

// resolveAccount resolves the ONE domain.Account this node binds for the given
// legacy mode (the back-compat input until the CLI carries the two axes in a
// later phase): signal -> a synthetic sim account; paper -> the moomoo SIMULATE
// account (PaperAccID); live -> the moomoo REAL account (LiveAccID). This is the
// single point where the paper-vs-live account axis is derived; everything
// downstream (executor TrdEnv, persistence account_id, reconciler) flows from it.
func (l *Live) resolveAccount(mode string) domain.Account {
	switch mode {
	case modePaper:
		return domain.NewBrokerAccount("moomoo", domain.EnvSimulate, l.cfg.PaperAccID, "")
	case modeLive:
		return domain.NewBrokerAccount("moomoo", domain.EnvReal, l.cfg.LiveAccID, "")
	default:
		return domain.SimAccount("signal")
	}
}

// ensureAccount UPSERTs acct into tms.accounts (idempotent) so the FK from
// tms.sessions.account_id is satisfiable, and returns the FK param (the account
// id, or SQL NULL when the pool is nil / id empty). It is the single place the
// session-bound account row is materialised, before the session row is inserted.
func (l *Live) ensureAccount(ctx context.Context, acct domain.Account) (any, error) {
	if l.pool == nil || acct.ID == "" {
		return nil, nil
	}
	if err := acct.Validate(); err != nil {
		return nil, fmt.Errorf("runner: invalid bound account: %w", err)
	}
	if _, err := l.pool.Exec(ctx, `
		INSERT INTO tms.accounts (id, venue, env, broker_acc_id, label)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (id) DO UPDATE SET
		    label      = EXCLUDED.label,
		    updated_at = now()`,
		acct.ID, acct.Venue, string(acct.Env), int64(acct.BrokerAccID), acct.Label); err != nil {
		return nil, fmt.Errorf("runner: upsert account %s: %w", acct.ID, err)
	}
	return acct.ID, nil
}

// syncSessionAccount makes the reused tms.sessions row's account_id match acct,
// the account this (re)built session attributes its broker rows to. It upserts
// acct first (idempotent — the FK target must exist before the UPDATE) and is a
// no-op when there is no pool (tests) or acct carries no id. Called on every
// session (re)build so an account-changing mode-switch never leaves the single
// reused session row pointing at the prior mode's account_id.
func (l *Live) syncSessionAccount(ctx context.Context, sessionID int64, acct domain.Account) error {
	if l.pool == nil || sessionID == 0 || acct.ID == "" {
		return nil
	}
	if _, err := l.ensureAccount(ctx, acct); err != nil {
		return err
	}
	if _, err := l.pool.Exec(ctx,
		`UPDATE tms.sessions SET account_id=$2 WHERE id=$1`,
		sessionID, acct.ID); err != nil {
		return fmt.Errorf("runner: syncing session account_id: %w", err)
	}
	return nil
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

// subscriptionCap resolves the moomoo OpenD per-connection K-line subscription
// quota the LIVE assembly must size the universe against: the configured
// MoomooMaxSub (TMS_MOOMOO_MAX_SUB) or moomoo.DefaultMaxSubscriptions (100) when
// unset. The shared universe.ResolveLiveSubscriptionSet reserves
// universe.SubscriptionSafetyMargin below this when sizing the SEPA top-N, so the
// realised distinct subscription set sits strictly below the hard OpenD limit and
// a freshly started multi live node never trips moomoo's "subscribing N would
// exceed cap" rejection at subscribe time.
func (l *Live) subscriptionCap() int {
	if l.cfg.MoomooMaxSub > 0 {
		return l.cfg.MoomooMaxSub
	}
	return moomoo.DefaultMaxSubscriptions
}

// ---------------------------------------------------------------------------
// tms.sessions + tms.halts persistence
// ---------------------------------------------------------------------------

// openSession inserts a RUNNING session row (one per trader id by the partial
// unique index). A pre-existing running row is reused (idempotent restart).
func (l *Live) openSession(ctx context.Context) (int64, error) {
	// Resolve + ensure the bound account row exists BEFORE the session row so the
	// sessions.account_id FK is satisfiable. The 2D model is persisted as
	// exec_policy (signal/auto) + the bound account env (via account_id); the
	// paper-vs-live distinction lives in the account, not the row's policy.
	mode := l.Mode()
	acct := l.resolveAccount(mode)
	accountIDParam, err := l.ensureAccount(ctx, acct)
	if err != nil {
		return 0, err
	}
	var id int64
	err = l.pool.QueryRow(ctx,
		`INSERT INTO tms.sessions (trader_id, exec_policy, account_id, status)
		 VALUES ($1, $2, $3, 'RUNNING') RETURNING id`,
		l.cfg.TraderID, string(execPolicyForMode(mode)), accountIDParam).Scan(&id)
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

// recordHalt inserts an active tms.halts row scoped to this node's session
// (best-effort audit; the session_id lets rehydrateHalt restore it on restart).
// Idempotent on the halt LATCH: if an active (cleared_at IS NULL) halt already
// exists for the session it is NOT duplicated, mirroring HaltState.Halt which
// keeps the first trigger (so a re-halt of an already-halted node does not stack
// rows that rehydration would have to disambiguate).
func (l *Live) recordHalt(ctx context.Context, kind commands.HaltKind, reason string) {
	if reason == "" {
		reason = string(kind)
	}
	if _, err := l.pool.Exec(ctx,
		`INSERT INTO tms.halts (session_id, kind, reason)
		 SELECT $1, $2, $3
		 WHERE NOT EXISTS (
		     SELECT 1 FROM tms.halts WHERE session_id=$1 AND cleared_at IS NULL)`,
		l.sessionIDValue(), string(kind), reason); err != nil {
		l.log.Warn().Err(err).Msg("recording halt row failed")
	}
}

// clearHalt clears any active tms.halts rows for this session (best-effort).
func (l *Live) clearHalt(ctx context.Context, by string) {
	if _, err := l.pool.Exec(ctx,
		`UPDATE tms.halts SET cleared_at=now(), cleared_by=$2
		  WHERE session_id=$1 AND cleared_at IS NULL`,
		l.sessionIDValue(), by); err != nil {
		l.log.Warn().Err(err).Msg("clearing halt rows failed")
	}
}

// sessionIDValue returns the open session id (or NULL via any when unset). A halt
// recorded before openSession (no path does this today) still persists as a
// session-less audit row rather than failing the write.
func (l *Live) sessionIDValue() any {
	l.mu.RLock()
	id := l.sessionID
	l.mu.RUnlock()
	if id == 0 {
		return nil
	}
	return id
}

// rehydrateHalt restores a latched halt from the durable tms.halts row on
// (re)start: durable state in Postgres is authoritative (the database-oriented
// thesis), so a MANUAL/reconciliation/broker/data halt set before a crash must
// survive the restart rather than being silently cleared by the fresh in-memory
// HaltState. Scoped to THIS session (the one openSession reused), so only a halt
// that belongs to the resumed session is re-applied. The daily-loss halt is NOT
// the concern here (the gate re-derives it every PostTimestamp); we still restore
// it for fidelity, and HaltState.Halt is idempotent if the gate re-latches it.
func (l *Live) rehydrateHalt(ctx context.Context) {
	id := l.sessionIDValue()
	if id == nil {
		return
	}
	var kind, reason string
	err := l.pool.QueryRow(ctx,
		`SELECT kind, reason FROM tms.halts
		  WHERE session_id=$1 AND cleared_at IS NULL
		  ORDER BY triggered_at DESC LIMIT 1`, id).Scan(&kind, &reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return // no active halt — fresh running state is correct
	}
	if err != nil {
		l.log.Warn().Err(err).Msg("rehydrating halt from PG failed (continuing with fresh state)")
		return
	}
	l.halt.Halt(commands.HaltKind(kind), reason)
	l.log.Warn().Str("kind", kind).Str("reason", reason).
		Msg("rehydrated active halt from PG on restart (durable state is authoritative; emitting/trading suppressed until Resume)")
}

// compile-time check: *Live is a commands.Controller.
var _ commands.Controller = (*Live)(nil)
