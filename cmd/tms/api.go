package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/spf13/cobra"

	"github.com/byjackchen/trade-tms-go/internal/api"
	"github.com/byjackchen/trade-tms-go/internal/apistore"
	"github.com/byjackchen/trade-tms-go/internal/app"
	"github.com/byjackchen/trade-tms-go/internal/commands"
	"github.com/byjackchen/trade-tms-go/internal/config"
	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/data/universe"
	"github.com/byjackchen/trade-tms-go/internal/db"
	"github.com/byjackchen/trade-tms-go/internal/hyperopt/study"
	"github.com/byjackchen/trade-tms-go/internal/jobs"
	"github.com/byjackchen/trade-tms-go/internal/params/paramsdb"
	"github.com/byjackchen/trade-tms-go/internal/runs"
	"github.com/byjackchen/trade-tms-go/internal/scheduler"
)

// newAPICmd implements `tms api`: the HTTP/WebSocket API for the UI
// (contract: docs/api.md). Container-internal port 8080 (TMS_API_ADDR),
// host port 18080 via compose.
//
// Health model (same pattern as `tms worker`): the runtime image is
// distroless, so the compose healthcheck execs `tms api --health`, which
// GETs the server's own /healthz over loopback and exits 0/1.
func newAPICmd(env *runtimeEnv) *cobra.Command {
	var (
		healthProbe bool
		addr        string
	)
	cmd := &cobra.Command{
		Use:   "api",
		Short: "Serve the HTTP/WebSocket API for the UI",
		Long: "Serves the UI-facing REST + WebSocket API (contract: docs/api.md):\n" +
			"data coverage/freshness/gaps, ticker search, dataset-sync history,\n" +
			"job enqueue/inspect/cancel, universe snapshots, and a WebSocket\n" +
			"fan-out of job/sync events bridged from Redis pub/sub.\n\n" +
			"Every /api/* route requires the TMS_API_TOKEN bearer token; /healthz\n" +
			"and /version are public.\n\n" +
			"With --health, instead probes a running server's /healthz endpoint\n" +
			"and exits 0 (serving) or 1 — used as the container healthcheck.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if addr == "" {
				addr = env.cfg.APIAddr
			}
			if healthProbe {
				return probeAPIHealth(cmd.Context(), addr)
			}
			return runAPI(cmd.Context(), env, addr)
		},
	}
	cmd.Flags().BoolVar(&healthProbe, "health", false, "probe a running api server's /healthz and exit 0/1 (container healthcheck mode)")
	cmd.Flags().StringVar(&addr, "addr", "", "listen/probe address (default: TMS_API_ADDR)")
	return cmd
}

func runAPI(ctx context.Context, env *runtimeEnv, addr string) error {
	log := env.log.With().Str("cmd", "api").Logger()

	// Fail fast without a token: an unauthenticated API is a
	// misconfiguration, not a degraded mode.
	token, err := env.cfg.Require("TMS_API_TOKEN",
		"set a strong random bearer token; the UI sends it as Authorization: Bearer <token>")
	if err != nil {
		return err
	}

	pool, err := db.NewPool(ctx, env.cfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	cal, err := calendar.NewNYSE()
	if err != nil {
		return err
	}

	// Redis is best-effort: without it the API still serves every REST
	// endpoint (the durable queue lives in PostgreSQL); only live WS events
	// and enqueue notifications degrade. /healthz reports the outage.
	redisClient := redis.NewClient(&redis.Options{
		Addr:     env.cfg.RedisAddr,
		DB:       env.cfg.RedisDB,
		Password: env.cfg.RedisPassword,
	})
	defer func() { _ = redisClient.Close() }()

	var queueOpts []jobs.Option
	pingCtx, cancelPing := context.WithTimeout(ctx, 3*time.Second)
	if err := redisClient.Ping(pingCtx).Err(); err != nil {
		log.Warn().Err(err).Str("addr", env.cfg.RedisAddr).
			Msg("redis unreachable at startup; ws events degraded until it returns")
	} else {
		queueOpts = append(queueOpts, jobs.WithNotifier(
			jobs.NewRedisNotifier(redisClient, jobs.DefaultEventsChannel, log)))
	}
	cancelPing()

	queue, err := jobs.NewQueue(pool, log, queueOpts...)
	if err != nil {
		return err
	}

	// Sync-now forcer (POST /api/v1/data/sync-now): a loop-less scheduler over
	// the same queue + ledger + NYSE calendar. It only ENQUEUES the daily
	// pipeline (the worker runs the actual Nasdaq sync), so no API key is
	// needed here. A misconfigured TMS_SCHEDULER_DAILY_AT/TZ degrades the
	// force endpoint to 503 rather than failing the whole API.
	syncForcer, err := buildSyncForcer(cal, queue, pool, log, env.cfg)
	if err != nil {
		log.Warn().Err(err).Msg("sync-now disabled (scheduler config invalid); POST /api/v1/data/sync-now will 503")
		syncForcer = nil
	}

	pgStore := apistore.NewPGStore(pool)
	srv, err := api.NewServer(api.Deps{
		Log:         log,
		Token:       token,
		CORSOrigins: env.cfg.APICORSOrigins,
		Jobs:        queue,
		Data:        pgStore,
		Universe:    universe.NewStore(pool),
		Runs:        runs.NewStore(pool),
		Strategies:  api.NewStrategyReader(paramsdb.NewReader(pool), env.cfg.StrategyParamsDir),
		Hyperopt:    study.NewStore(pool),
		Promoter:    study.NewPromoter(pool),
		Calendar:    cal,
		PingPG:      pool.Ping,
		PingRedis:   func(ctx context.Context) error { return redisClient.Ping(ctx).Err() },
		Live:        apistore.NewLiveStore(pool),
		// SystemReader backs GET /api/v1/system (queue depth, active sessions,
		// data freshness); the same PGStore satisfies it.
		System: pgStore,
		// AuditReader backs GET /api/v1/audit (the append-only operational audit
		// trail the Ops UI renders); the same PGStore satisfies it.
		Audit: pgStore,
		// The audited command enqueuer is the ONLY write path of the live API.
		// A live/paper mode switch requires the confirmation token; "" means the
		// presence of any non-empty confirm_token suffices (the API gates by
		// presence — a stronger token can be wired later via app_config).
		Commands: commands.NewEnqueuer(pool, redisClient, ""),
		// Sync forces the daily incremental-sync pipeline immediately
		// (POST /api/v1/data/sync-now); nil here degrades that endpoint to 503.
		Sync: syncForcer,
	})
	if err != nil {
		return err
	}

	// Redis -> WS bridge: job/sync pub-sub events. Retries internally with
	// backoff; follows ctx, no explicit shutdown beyond hub close.
	bridgeDone := make(chan struct{})
	go func() {
		defer close(bridgeDone)
		api.RunEventBridge(ctx, redisClient, srv.Hub(), log)
	}()

	// Live-stream bridge: tails the per-trader Redis streams
	// (trader-{id}:stream:{topic}) and fans live cockpit updates to the hub. The
	// trader id is TMS_LIVE_TRADER_ID (or the default signal trader); the bridge
	// is a no-op without Redis. Follows ctx.
	liveTrader := strings.TrimSpace(os.Getenv("TMS_LIVE_TRADER_ID"))
	if liveTrader == "" {
		liveTrader = "SIGNAL-001"
	}
	liveBridgeDone := make(chan struct{})
	go func() {
		defer close(liveBridgeDone)
		api.RunLiveStreamBridge(ctx, redisClient, srv.Hub(), liveTrader, log)
	}()

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("api: listener on %s: %w", addr, err)
	}
	serveErr := make(chan error, 1)
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
		close(serveErr)
	}()
	log.Info().Str("addr", ln.Addr().String()).
		Strs("cors_origins", env.cfg.APICORSOrigins).
		Msg("api server listening")

	select {
	case err, ok := <-serveErr:
		if ok && err != nil {
			return fmt.Errorf("api: server stopped unexpectedly: %w", err)
		}
		return errors.New("api: server stopped unexpectedly")
	case <-ctx.Done():
	}

	log.Info().Msg("shutdown signal received; draining")
	shutdownErr := app.GracefulShutdown(log, 15*time.Second,
		app.ShutdownFunc{Name: "http-server", Fn: func(c context.Context) error {
			if err := httpSrv.Shutdown(c); err != nil && !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		}},
		app.ShutdownFunc{Name: "ws-hub", Fn: srv.Hub().Close},
		app.ShutdownFunc{Name: "event-bridge", Fn: func(c context.Context) error {
			select {
			case <-bridgeDone:
				return nil
			case <-c.Done():
				return c.Err()
			}
		}},
		app.ShutdownFunc{Name: "live-bridge", Fn: func(c context.Context) error {
			select {
			case <-liveBridgeDone:
				return nil
			case <-c.Done():
				return c.Err()
			}
		}},
	)
	return shutdownErr
}

// syncForcerAdapter bridges *scheduler.Scheduler.SyncNow (returning a
// scheduler.TickResult) to the api.SyncForcer seam (returning an
// api.SyncNowResult), keeping the scheduler package out of the api package's
// imports.
type syncForcerAdapter struct {
	sched *scheduler.Scheduler
	log   zerolog.Logger
}

// SyncNow implements api.SyncForcer.
func (a *syncForcerAdapter) SyncNow(ctx context.Context, actor string) (api.SyncNowResult, error) {
	res, err := a.sched.SyncNow(ctx, actor)
	if err != nil {
		return api.SyncNowResult{}, err
	}
	return api.SyncNowResult{
		TradingDate: res.Date.String(),
		Forced:      res.Action == scheduler.ActionEnqueued,
		DataJobID:   res.Pipeline.DataJobID,
		EODJobID:    res.Pipeline.EODJobID,
	}, nil
}

// buildSyncForcer constructs the loop-less scheduler that backs
// POST /api/v1/data/sync-now over the API's existing queue + pool + calendar.
// It only enqueues jobs (the worker runs the actual sync), so no API key is
// required. An invalid TMS_SCHEDULER_DAILY_AT/TZ yields an error and the API
// disables the endpoint (503) rather than refusing to start.
func buildSyncForcer(cal *calendar.Calendar, queue *jobs.Queue, pool *pgxpool.Pool, log zerolog.Logger, cfg *config.Config) (api.SyncForcer, error) {
	at, err := scheduler.ParseTimeOfDay(cfg.SchedulerDailyAt)
	if err != nil {
		return nil, err
	}
	loc, err := time.LoadLocation(cfg.SchedulerTZ)
	if err != nil {
		return nil, fmt.Errorf("invalid TMS_SCHEDULER_TZ %q: %w", cfg.SchedulerTZ, err)
	}
	ledger, err := scheduler.NewPGLedger(pool)
	if err != nil {
		return nil, err
	}
	host, _ := os.Hostname()
	if host == "" {
		host = "api"
	}
	sched, err := scheduler.New(cal, queue, ledger, log, scheduler.Options{
		DailyAt: at, Loc: loc, InstanceID: "api:" + host,
	})
	if err != nil {
		return nil, err
	}
	return &syncForcerAdapter{sched: sched, log: log}, nil
}

// probeAPIHealth is the --health container-healthcheck mode: GET /healthz
// on the listen address (wildcard hosts probe via loopback — the probe
// runs inside the same container netns) with a short deadline; exit 0 on
// HTTP 200.
func probeAPIHealth(ctx context.Context, addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("api health probe: invalid address %q: %w", addr, err)
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
		return fmt.Errorf("api health probe: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("api health probe: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("api health probe: %s returned %s", url, resp.Status)
	}
	return nil
}
