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

	"github.com/byjackchen/trade-tms-go/internal/app"
	"github.com/byjackchen/trade-tms-go/internal/db"
	"github.com/byjackchen/trade-tms-go/internal/livengine"
	"github.com/byjackchen/trade-tms-go/internal/runner"
)

// newLiveCmd implements `tms live --mode signal`: the live (real-time) trading
// node. It wires the native moomoo OpenD client (or the protocol-faithful mock,
// switched by TMS_MOOMOO_ADDR) -> a streaming feed -> the SAME internal/core
// engine + strategy / portfolio / warmup code as backtest, driven by a wall
// clock, recording a SignalIntent per strategy per bar (tms.signal_intents +
// Redis streams) and submitting NO orders — all under the ops.commands control
// plane (halt/resume/kill/stop/set_mode) with full audit and a graceful
// lifecycle (ctx cancellation, drain, no goroutine leaks, structured logs, no
// secrets logged).
//
// Only --mode signal is accepted; paper/live (order submission, fills, FLATTEN)
// are deferred to P6 (locked decision 1 + 6).
func newLiveCmd(env *runtimeEnv) *cobra.Command {
	var (
		modeStr      string
		traderID     string
		strategy     string
		tickersCSV   string
		orbSymbol    string
		moomooAddr   string
		startBalance float64
		barSeconds   int
		healthAddr   string
		healthProbe  bool
		drainTimeout time.Duration
	)

	cmd := &cobra.Command{
		Use:   "live",
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
				return probeLiveHealth(cmd.Context(), healthAddr)
			}
			return runLive(cmd.Context(), env, liveArgs{
				mode:         strings.TrimSpace(modeStr),
				traderID:     strings.TrimSpace(traderID),
				strategy:     strings.TrimSpace(strategy),
				tickersCSV:   tickersCSV,
				orbSymbol:    strings.TrimSpace(orbSymbol),
				moomooAddr:   strings.TrimSpace(moomooAddr),
				startBalance: startBalance,
				barSeconds:   barSeconds,
				healthAddr:   healthAddr,
				drainTimeout: drainTimeout,
			})
		},
	}

	cmd.Flags().StringVar(&modeStr, "mode", "signal", "execution mode: signal (paper/live deferred to P6)")
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
	return cmd
}

type liveArgs struct {
	mode         string
	traderID     string
	strategy     string
	tickersCSV   string
	orbSymbol    string
	moomooAddr   string
	startBalance float64
	barSeconds   int
	healthAddr   string
	drainTimeout time.Duration
}

// runLive assembles and runs the live signal-mode trading node (P5): the native
// moomoo OpenD client (or mock by TMS_MOOMOO_ADDR) -> a streaming feed -> the
// same internal/core engine + strategy/portfolio/warmup as backtest, driven by
// a wall clock, recording a SignalIntent per strategy per bar to PG + Redis and
// submitting NO orders, under the ops.commands control plane.
func runLive(parent context.Context, env *runtimeEnv, a liveArgs) error {
	log := env.log.With().
		Str("cmd", "live").
		Str("mode", a.mode).
		Str("trader_id", a.traderID).
		Str("strategy", a.strategy).
		Logger()

	mode := livengine.Mode(a.mode)
	if !mode.IsValid() {
		return fmt.Errorf("--mode %q invalid (want signal|paper|live)", a.mode)
	}
	if mode != livengine.ModeSignal {
		return fmt.Errorf("--mode %q not wired yet: P5 is signal-only; paper/live (orders, fills, flatten) is deferred to P6", a.mode)
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

	node, err := runner.NewLive(pool, redisClient, runner.LiveConfig{
		TraderID:        a.traderID,
		Mode:            a.mode,
		Strategy:        a.strategy,
		Tickers:         tickers,
		ORBSymbol:       a.orbSymbol,
		StartingBalance: a.startBalance,
		MoomooAddr:      moomooAddr,
		MoomooMaxSub:    env.cfg.MoomooMaxSub,
		BarSeconds:      a.barSeconds,
		ParamsDir:       env.cfg.StrategyParamsDir,
		DrainTimeout:    a.drainTimeout,
	}, log)
	if err != nil {
		return err
	}

	// Liveness HTTP server (compose healthcheck on :18090 via host port).
	healthSrv, err := startLiveHealthServer(a.healthAddr, node, log)
	if err != nil {
		return err
	}

	log.Info().
		Str("moomoo_addr", moomooAddr).
		Float64("health_nav", a.startBalance).
		Msg("live node starting (signal mode)")

	runErr := node.Run(ctx)

	shutdownErr := app.GracefulShutdown(log, a.drainTimeout,
		app.ShutdownFunc{Name: "live-health", Fn: healthSrv.Shutdown},
		app.ShutdownFunc{Name: "live-node", Fn: func(context.Context) error {
			log.Info().Msg("live node stopped")
			return nil
		}},
	)
	return errors.Join(runErr, shutdownErr)
}

// liveHealthServer serves the live node's liveness + control state.
type liveHealthServer struct {
	srv *http.Server
}

// Shutdown gracefully stops the health server.
func (s *liveHealthServer) Shutdown(ctx context.Context) error { return s.srv.Shutdown(ctx) }

// startLiveHealthServer serves GET /healthz with the node's mode + halt state
// on addr (the compose healthcheck targets the internal :18090 via host port).
func startLiveHealthServer(addr string, node *runner.Live, log zerolog.Logger) (*liveHealthServer, error) {
	hlog := log.With().Str("component", "live-health").Logger()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("live: health listener on %s: %w", addr, err)
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
			hlog.Error().Err(err).Msg("live health server stopped unexpectedly")
		}
	}()
	hlog.Info().Str("addr", ln.Addr().String()).Msg("live health endpoint listening")
	return &liveHealthServer{srv: srv}, nil
}

// probeLiveHealth is the --health container-healthcheck mode: GET /healthz on
// addr with a short deadline; exit 0 on HTTP 200.
func probeLiveHealth(ctx context.Context, addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("live health probe: invalid address %q: %w", addr, err)
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
		return fmt.Errorf("live health probe: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("live health probe: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("live health probe: %s returned %s", url, resp.Status)
	}
	return nil
}
