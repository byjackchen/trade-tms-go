package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/spf13/cobra"

	"github.com/byjackchen/trade-tms-go/internal/adapters/moomoo"
	"github.com/byjackchen/trade-tms-go/internal/app"
	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/db"
	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/preflight"
	"github.com/byjackchen/trade-tms-go/internal/runner"
)

// newTradeRunCmd implements `tms trade run --mode signal`: the live (real-time)
// trading node. It wires the native moomoo OpenD client (or the protocol-faithful
// mock, switched by TMS_MOOMOO_ADDR) -> a streaming feed -> the SAME internal/core
// engine + strategy / portfolio / warmup code as backtest, driven by a wall
// clock, recording a SignalIntent per strategy per bar (tms.signal_intents +
// Redis streams) and submitting NO orders — all under the ops.commands control
// plane (halt/resume/kill/stop/set_mode) with full audit and a graceful
// lifecycle (ctx cancellation, drain, no goroutine leaks, structured logs, no
// secrets logged).
//
// Only --mode signal is accepted; paper/live (order submission, fills, FLATTEN)
// are deferred to P6 (locked decision 1 + 6).
func newTradeRunCmd(env *runtimeEnv) *cobra.Command {
	var (
		modeStr       string
		traderID      string
		strategy      string
		tickersCSV    string
		orbSymbol     string
		moomooAddr    string
		startBalance  float64
		barSeconds    int
		healthAddr    string
		healthProbe   bool
		drainTimeout  time.Duration
		skipPreflight bool
		maxStaleDays  int
		manualMode    string
		manualAPIAddr string
	)

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run the live trading node (signal mode; paper/live deferred to P6)",
		Long: "Runs the live (real-time) engine: the SAME internal/core event loop and\n" +
			"strategy / portfolio / warmup code as backtest, driven by a wall clock and a\n" +
			"streaming moomoo (or mock OpenD) bar feed. In signal mode it records a\n" +
			"SignalIntent per strategy per bar (tms.signal_intents + Redis streams) and\n" +
			"submits NO orders, under the ops.commands control plane (halt/resume/kill/\n" +
			"stop/set_mode) with full audit. With --health, probes a running node's\n" +
			"/healthz and exits 0/1 (container healthcheck mode).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if healthAddr == "" {
				healthAddr = env.cfg.WorkerHealthAddr
			}
			if healthProbe {
				return probeTradeHealth(cmd.Context(), healthAddr)
			}
			return runTradeRun(cmd.Context(), env, tradeRunArgs{
				mode:          strings.TrimSpace(modeStr),
				traderID:      strings.TrimSpace(traderID),
				strategy:      strings.TrimSpace(strategy),
				tickersCSV:    tickersCSV,
				orbSymbol:     strings.TrimSpace(orbSymbol),
				moomooAddr:    strings.TrimSpace(moomooAddr),
				startBalance:  startBalance,
				barSeconds:    barSeconds,
				healthAddr:    healthAddr,
				drainTimeout:  drainTimeout,
				skipPreflight: skipPreflight,
				maxStaleDays:  maxStaleDays,
				manualMode:    strings.TrimSpace(manualMode),
				manualAPIAddr: strings.TrimSpace(manualAPIAddr),
			})
		},
	}

	cmd.Flags().StringVar(&modeStr, "mode", "signal", "execution mode: signal | paper | live (live requires real acc id + TMS_LIVE_CONFIRM + the TMS-LIVE-REAL-001 trader id)")
	cmd.Flags().StringVar(&traderID, "trader-id", "SIGNAL-001", "trader id (Redis namespace + sessions.trader_id)")
	cmd.Flags().StringVar(&strategy, "strategy", "multi", "strategy: sepa | sector_rotation | pairs | orb | multi")
	cmd.Flags().StringVar(&tickersCSV, "tickers", "", "comma-separated stock universe (SEPA/multi); strategy derives ETFs/legs/SPY from params")
	cmd.Flags().StringVar(&orbSymbol, "orb-symbol", "", "ORB strategy: the single intraday instrument symbol")
	cmd.Flags().StringVar(&moomooAddr, "moomoo-addr", "", "moomoo OpenD address (default TMS_MOOMOO_ADDR; mock or host.docker.internal:11111)")
	cmd.Flags().Float64Var(&startBalance, "starting-balance", 100000.0, "informational health-NAV starting balance (signal mode has no account)")
	cmd.Flags().IntVar(&barSeconds, "bar-seconds", 86400, "live K-line width in seconds (86400 daily; 60/300/900/1800/3600 intraday)")
	cmd.Flags().StringVar(&healthAddr, "health-addr", "", "liveness HTTP listen/probe address (default TMS_WORKER_HEALTH_ADDR)")
	cmd.Flags().BoolVar(&healthProbe, "health", false, "probe a running live node's /healthz and exit 0/1 (container healthcheck mode)")
	cmd.Flags().DurationVar(&drainTimeout, "drain-timeout", 10*time.Second, "max wait for in-flight work on shutdown")
	cmd.Flags().BoolVar(&skipPreflight, "skip-preflight", false, "DANGER: start without the go-live preflight gate (paper/signal only; REFUSED for --mode live)")
	cmd.Flags().IntVar(&maxStaleDays, "max-stale-days", 1, "DATA_CURRENT tolerance: max trading days the data frontier may lag T-1")
	cmd.Flags().StringVar(&manualMode, "manual-mode", "", "connect an operator MANUAL trade desk: paper | live (independent of --mode; live requires the full 4-factor activation). Serves /api/v1/trade/* on --manual-api-addr")
	cmd.Flags().StringVar(&manualAPIAddr, "manual-api-addr", "127.0.0.1:18091", "MANUAL trade desk HTTP listen address (bearer-guarded by TMS_API_TOKEN)")

	return cmd
}

// newTradePreflightCmd implements `tms trade preflight`: run the go-live precondition
// checks for a session (mode/strategy/tickers) and print a PASS/FAIL table. Exits 0
// when all BLOCKER checks pass, 1 otherwise — the machine-enforceable go/no-go gate
// the operator (and CI) can run before a paper/live session.
func newTradePreflightCmd(env *runtimeEnv) *cobra.Command {
	var (
		modeStr      string
		strategy     string
		tickersCSV   string
		orbSymbol    string
		moomooAddr   string
		maxStaleDays int
		checkOpenD   bool
		asJSON       bool
	)
	cmd := &cobra.Command{
		Use:   "preflight",
		Short: "Verify go-live preconditions (data freshness, warmup, caps, universe, OpenD, PG/Redis)",
		Long: "Runs the structured go-live PREFLIGHT for a session and prints a PASS/FAIL\n" +
			"table. Each check reports pass | warn | fail with a blocker | warn severity.\n" +
			"Exit 0 when every BLOCKER passes, 1 otherwise. signal mode treats data\n" +
			"freshness + OpenD as advisory (warn); paper/live require all blockers. This\n" +
			"is the same report 'tms trade run' enforces at startup and GET /api/v1/trade/preflight serves.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runTradePreflight(cmd.Context(), env, preflightArgs{
				mode:         strings.TrimSpace(modeStr),
				strategy:     strings.TrimSpace(strategy),
				tickersCSV:   tickersCSV,
				orbSymbol:    strings.TrimSpace(orbSymbol),
				moomooAddr:   strings.TrimSpace(moomooAddr),
				maxStaleDays: maxStaleDays,
				checkOpenD:   checkOpenD,
				asJSON:       asJSON,
			})
		},
	}
	cmd.Flags().StringVar(&modeStr, "mode", "signal", "session mode: signal | paper | live")
	cmd.Flags().StringVar(&strategy, "strategy", "multi", "strategy: sepa | sector_rotation | pairs | orb | multi")
	cmd.Flags().StringVar(&tickersCSV, "tickers", "", "comma-separated SEPA stock universe (sepa/multi); empty resolves the default SF1 window universe")
	cmd.Flags().StringVar(&orbSymbol, "orb-symbol", "", "ORB strategy: the single intraday instrument symbol")
	cmd.Flags().StringVar(&moomooAddr, "moomoo-addr", "", "moomoo OpenD address for the OpenD probe (default TMS_MOOMOO_ADDR)")
	cmd.Flags().IntVar(&maxStaleDays, "max-stale-days", 1, "DATA_CURRENT tolerance: max trading days the data frontier may lag T-1")
	cmd.Flags().BoolVar(&checkOpenD, "check-opend", false, "probe OpenD even in signal mode (paper/live always probe it)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the report as JSON instead of the table")
	return cmd
}

type tradeRunArgs struct {
	mode          string
	traderID      string
	strategy      string
	tickersCSV    string
	orbSymbol     string
	moomooAddr    string
	startBalance  float64
	barSeconds    int
	healthAddr    string
	drainTimeout  time.Duration
	skipPreflight bool
	maxStaleDays  int
	manualMode    string
	manualAPIAddr string
}

// runTradeRun assembles and runs the live signal-mode trading node (P5): the native
// moomoo OpenD client (or mock by TMS_MOOMOO_ADDR) -> a streaming feed -> the
// same internal/core engine + strategy/portfolio/warmup as backtest, driven by
// a wall clock, recording a SignalIntent per strategy per bar to PG + Redis and
// submitting NO orders, under the ops.commands control plane.
func runTradeRun(parent context.Context, env *runtimeEnv, a tradeRunArgs) error {
	log := env.log.With().
		Str("cmd", "trade run").
		Str("mode", a.mode).
		Str("trader_id", a.traderID).
		Str("strategy", a.strategy).
		Logger()

	mode := domain.Mode(a.mode)
	if !mode.IsValid() {
		return fmt.Errorf("--mode %q invalid (want signal|paper|live)", a.mode)
	}
	if a.traderID == "" {
		return fmt.Errorf("--trader-id is required (Redis namespace + sessions.trader_id)")
	}
	if a.startBalance <= 0 {
		return fmt.Errorf("--starting-balance must be positive (informational health NAV)")
	}

	// moomoo address resolution: --moomoo-addr, else TMS_MOOMOO_ADDR (the
	// real-vs-mock switch, P5 decision 2), else config / local OpenD default.
	moomooAddr := a.moomooAddr
	if moomooAddr == "" {
		moomooAddr = strings.TrimSpace(os.Getenv("TMS_MOOMOO_ADDR"))
	}
	if moomooAddr == "" {
		moomooAddr = env.cfg.MoomooAddr
	}
	if moomooAddr == "" {
		moomooAddr = "127.0.0.1:11111"
	}

	// Lifecycle context: cancelled on SIGINT/SIGTERM (graceful first, forceful
	// on a second signal — app.SignalContext contract).
	ctx, stop := app.SignalContext(parent)
	defer stop()

	pool, err := db.NewPool(ctx, env.cfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	cal, err := calendar.NewNYSE()
	if err != nil {
		return fmt.Errorf("trade: building NYSE calendar: %w", err)
	}

	// Redis is best-effort transport (decision 5): without it the node still
	// records intents to PG and the command consumer polls (no Redis notify).
	redisClient := redis.NewClient(&redis.Options{
		Addr:     env.cfg.RedisAddr,
		DB:       env.cfg.RedisDB,
		Password: env.cfg.RedisPassword,
	})
	defer func() { _ = redisClient.Close() }()
	if perr := redisClient.Ping(ctx).Err(); perr != nil {
		log.Warn().Err(perr).Msg("redis unreachable; live streams + command notify degraded (PG unaffected)")
		_ = redisClient.Close()
		redisClient = nil
	}

	var tickers []string
	for _, t := range strings.Split(a.tickersCSV, ",") {
		if t = strings.ToUpper(strings.TrimSpace(t)); t != "" {
			tickers = append(tickers, t)
		}
	}

	// GO-LIVE PREFLIGHT GATE: before starting a session, verify every precondition
	// (data freshness, per-strategy warmup, market caps, universe, OpenD, PG/Redis).
	// If any BLOCKER fails, REFUSE to start — the audit found three latent gaps
	// (SEPA-only warmup, stale data, the freshness bug) that a startup preflight
	// would have caught. --skip-preflight overrides with a loud warning (operators
	// who knowingly accept the risk; never the default).
	//
	// HARD GUARD (finding 1): --skip-preflight is NEVER honored for mode=live
	// (real money). Allowing an operator to start REAL-MONEY trading with zero
	// precondition verification is exactly the bypassable-blocker the preflight
	// exists to close. For live the preflight is mandatory and non-overridable;
	// paper/signal may still skip it (loudly).
	if a.skipPreflight && mode == domain.ModeLive {
		return fmt.Errorf("--skip-preflight is refused for mode=live: the go-live preflight is MANDATORY for real-money trading and cannot be bypassed (run `tms trade preflight --mode live ...` to see the failing blockers and resolve them)")
	}
	if a.skipPreflight {
		log.Warn().Str("mode", a.mode).Msg("PREFLIGHT SKIPPED (--skip-preflight): starting WITHOUT go-live precondition checks — blockers are NOT enforced (NOT permitted for mode=live)")
	} else {
		rep := preflight.Run(ctx, preflight.Config{
			Mode:                a.mode,
			Strategy:            a.strategy,
			Tickers:             tickers,
			ORBSymbol:           a.orbSymbol,
			MaxStaleTradingDays: a.maxStaleDays,
			// paper/live always probe OpenD; in signal mode we do too here (the node
			// is about to open a live OpenD socket regardless, so an unreachable broker
			// is worth surfacing — as a warn for signal, a blocker for paper/live).
			CheckOpenD: true,
		}, preflight.NewPGProbes(preflight.PGProbesConfig{
			Pool:      pool,
			Calendar:  cal,
			Redis:     redisClient,
			ParamsDir: env.cfg.StrategyParamsDir,
			MoomooCfg: moomoo.Options{Addr: moomooAddr, MaxSubscriptions: env.cfg.MoomooMaxSub, Logger: log},
			Log:       log,
		}))
		preflight.RenderTable(os.Stderr, rep) // human-readable table for the operator
		for _, w := range rep.Warnings() {
			log.Warn().Str("check", w.Check).Str("detail", w.Detail).Msg("preflight warning")
		}
		if !rep.OK {
			for _, b := range rep.Blockers() {
				log.Error().Str("check", b.Check).Str("detail", b.Detail).Msg("preflight BLOCKER failed")
			}
			// --skip-preflight is offered as an override ONLY for paper/signal; for
			// mode=live it is refused above, so do not advertise it as an option.
			if mode == domain.ModeLive {
				return fmt.Errorf("go-live preflight failed: %d blocker(s) must be resolved before real-money (live) trading", len(rep.Blockers()))
			}
			return fmt.Errorf("go-live preflight failed: %d blocker(s) must be resolved (or pass --skip-preflight to override for paper/signal)", len(rep.Blockers()))
		}
		log.Info().Int("checks", len(rep.Checks)).Int("warnings", len(rep.Warnings())).Msg("go-live preflight passed")
	}

	// Paper/live trading config from the secret env (never logged): the broker
	// account ids + the live-activation material (decision 8). Signal mode ignores
	// these; paper/live require them (NewLive enforces).
	paperAccID := parseUintEnv("TMS_MOOMOO_PAPER_ACC_ID")
	liveAccID := parseUintEnv("TMS_MOOMOO_LIVE_ACC_ID")
	reconcileEvery := parseDurationEnv("TMS_RECONCILE_INTERVAL", 5*time.Minute)

	node, err := runner.NewLive(pool, redisClient, runner.LiveConfig{
		TraderID:               a.traderID,
		Mode:                   a.mode,
		Strategy:               a.strategy,
		Tickers:                tickers,
		ORBSymbol:              a.orbSymbol,
		StartingBalance:        a.startBalance,
		MoomooAddr:             moomooAddr,
		MoomooMaxSub:           env.cfg.MoomooMaxSub,
		BarSeconds:             a.barSeconds,
		ParamsDir:              env.cfg.StrategyParamsDir,
		DrainTimeout:           a.drainTimeout,
		PaperAccID:             paperAccID,
		LiveAccID:              liveAccID,
		UnlockPassword:         strings.TrimSpace(os.Getenv("TMS_MOOMOO_UNLOCK_PASSWORD")),
		LiveConfirmationPhrase: strings.TrimSpace(os.Getenv("TMS_LIVE_CONFIRM")),
		ReconcileInterval:      reconcileEvery,
	}, log)
	if err != nil {
		return err
	}

	// Liveness HTTP server (compose healthcheck on :18090 via host port).
	healthSrv, err := startTradeHealthServer(a.healthAddr, node, log)
	if err != nil {
		return err
	}

	// Optional MANUAL trade desk: connect an operator-driven desk (paper/live) and
	// serve the /api/v1/trade/* mutation surface, independent of the strategy mode.
	// It runs in this process because it holds the broker connection.
	var manualSrv *manualTradeServer
	if a.manualMode != "" {
		manualSrv, err = startManualTradeServer(ctx, manualTradeServerArgs{
			node:    node,
			mode:    a.manualMode,
			apiAddr: a.manualAPIAddr,
			token:   env.cfg.APIToken,
			log:     log,
		})
		if err != nil {
			return err
		}
	}

	log.Info().
		Str("moomoo_addr", moomooAddr).
		Str("mode", a.mode).
		Str("manual_mode", a.manualMode).
		Float64("health_nav", a.startBalance).
		Msg("live node starting")

	runErr := node.Run(ctx)

	shutdownErr := app.GracefulShutdown(log, a.drainTimeout,
		app.ShutdownFunc{Name: "manual-trade-api", Fn: manualShutdown(manualSrv)},
		app.ShutdownFunc{Name: "live-health", Fn: healthSrv.Shutdown},
		app.ShutdownFunc{Name: "live-node", Fn: func(context.Context) error {
			log.Info().Msg("live node stopped")
			return nil
		}},
	)
	return errors.Join(runErr, shutdownErr)
}

// preflightArgs carries the `tms trade preflight` flags.
type preflightArgs struct {
	mode         string
	strategy     string
	tickersCSV   string
	orbSymbol    string
	moomooAddr   string
	maxStaleDays int
	checkOpenD   bool
	asJSON       bool
}

// runTradePreflight runs the go-live preflight for a session and prints the
// PASS/FAIL table (or JSON). It returns a non-nil error (so the process exits
// non-zero) iff any BLOCKER check failed — the machine-checkable go/no-go gate.
func runTradePreflight(parent context.Context, env *runtimeEnv, a preflightArgs) error {
	log := env.log.With().Str("cmd", "trade preflight").Str("mode", a.mode).Str("strategy", a.strategy).Logger()

	mode := domain.Mode(a.mode)
	if !mode.IsValid() {
		return fmt.Errorf("--mode %q invalid (want signal|paper|live)", a.mode)
	}

	moomooAddr := a.moomooAddr
	if moomooAddr == "" {
		moomooAddr = strings.TrimSpace(os.Getenv("TMS_MOOMOO_ADDR"))
	}
	if moomooAddr == "" {
		moomooAddr = env.cfg.MoomooAddr
	}
	if moomooAddr == "" {
		moomooAddr = "127.0.0.1:11111"
	}

	ctx, stop := app.SignalContext(parent)
	defer stop()

	pool, err := db.NewPool(ctx, env.cfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	cal, err := calendar.NewNYSE()
	if err != nil {
		return fmt.Errorf("preflight: building NYSE calendar: %w", err)
	}

	// Redis is probed (a blocker) — build a client without failing if it is down
	// (the check reports the outage; nil means "not configured" which also fails).
	redisClient := redis.NewClient(&redis.Options{
		Addr:     env.cfg.RedisAddr,
		DB:       env.cfg.RedisDB,
		Password: env.cfg.RedisPassword,
	})
	defer func() { _ = redisClient.Close() }()

	var tickers []string
	for _, t := range strings.Split(a.tickersCSV, ",") {
		if t = strings.ToUpper(strings.TrimSpace(t)); t != "" {
			tickers = append(tickers, t)
		}
	}

	rep := preflight.Run(ctx, preflight.Config{
		Mode:                a.mode,
		Strategy:            a.strategy,
		Tickers:             tickers,
		ORBSymbol:           a.orbSymbol,
		MaxStaleTradingDays: a.maxStaleDays,
		CheckOpenD:          a.checkOpenD,
	}, preflight.NewPGProbes(preflight.PGProbesConfig{
		Pool:      pool,
		Calendar:  cal,
		Redis:     redisClient,
		ParamsDir: env.cfg.StrategyParamsDir,
		MoomooCfg: moomoo.Options{Addr: moomooAddr, MaxSubscriptions: env.cfg.MoomooMaxSub, Logger: log},
		Log:       log,
	}))

	if a.asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return fmt.Errorf("preflight: encoding report: %w", err)
		}
	} else {
		preflight.RenderTable(os.Stdout, rep)
	}

	if !rep.OK {
		return fmt.Errorf("preflight FAILED: %d blocker(s)", len(rep.Blockers()))
	}
	return nil
}

// tradeHealthServer serves the trade node's liveness + control state.
type tradeHealthServer struct {
	srv *http.Server
}

// Shutdown gracefully stops the health server.
func (s *tradeHealthServer) Shutdown(ctx context.Context) error { return s.srv.Shutdown(ctx) }

// startTradeHealthServer serves GET /healthz with the node's mode + halt state
// on addr (the compose healthcheck targets the internal :18090 via host port).
func startTradeHealthServer(addr string, node *runner.Live, log zerolog.Logger) (*tradeHealthServer, error) {
	hlog := log.With().Str("component", "trade-health").Logger()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("trade: health listener on %s: %w", addr, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		snap := node.HaltState().Snapshot()
		sess := node.SessionHealth()
		// Liveness reflects BOTH the control state and the inner session:
		//   - stopped/killed       -> "stopping" (503): the node is draining.
		//   - session crash-looping -> "degraded" (503): the node cannot keep a
		//     session running (e.g. empty universe failing assembly on every
		//     restart). A green probe here would falsely tell an operator /
		//     `compose --wait` gate the node is emitting signals when it is doing
		//     nothing (finding 2).
		//   - halted-but-running    -> "ok" (200): an intentional pause, not a
		//     failure (bars keep state warm; only NEW intents are suppressed).
		status, code := "ok", http.StatusOK
		switch {
		case snap.Stopped:
			status, code = "stopping", http.StatusServiceUnavailable
		case !sess.Healthy:
			status, code = "degraded", http.StatusServiceUnavailable
		}
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":  status,
			"mode":    node.Mode(),
			"halt":    snap,
			"session": sess,
		})
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			hlog.Error().Err(err).Msg("trade health server stopped unexpectedly")
		}
	}()
	hlog.Info().Str("addr", ln.Addr().String()).Msg("trade health endpoint listening")
	return &tradeHealthServer{srv: srv}, nil
}

// parseUintEnv reads an unsigned-int env var, returning 0 when unset/invalid
// (the SAFE default: a missing acc id can only fall back to "not configured",
// never to a wrong account).
func parseUintEnv(key string) uint64 {
	s := strings.TrimSpace(os.Getenv(key))
	if s == "" {
		return 0
	}
	var v uint64
	if _, err := fmt.Sscanf(s, "%d", &v); err != nil {
		return 0
	}
	return v
}

// parseDurationEnv reads a duration env var, returning def when unset/invalid.
func parseDurationEnv(key string, def time.Duration) time.Duration {
	s := strings.TrimSpace(os.Getenv(key))
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return def
	}
	return d
}

// probeTradeHealth is the --health container-healthcheck mode: GET /healthz on
// addr with a short deadline; exit 0 on HTTP 200.
func probeTradeHealth(ctx context.Context, addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("trade health probe: invalid address %q: %w", addr, err)
	}
	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	url := "http://" + net.JoinHostPort(host, port) + "/healthz"
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("trade health probe: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("trade health probe: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("trade health probe: %s returned %s", url, resp.Status)
	}
	return nil
}
