package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/spf13/cobra"

	"github.com/byjackchen/trade-tms-go/internal/app"
	"github.com/byjackchen/trade-tms-go/internal/config"
	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/data/sharadar"
	"github.com/byjackchen/trade-tms-go/internal/db"
	"github.com/byjackchen/trade-tms-go/internal/jobs"
	"github.com/byjackchen/trade-tms-go/internal/jobs/handlers"
)

// newWorkerCmd implements `tms worker`: a pool of N concurrent job
// executors over the durable tms.jobs queue (internal/jobs), with
// heartbeat-based stale-claim recovery, cooperative cancel, panic
// isolation per job and graceful drain on SIGTERM.
//
// Health model (documented for compose): the worker serves
// GET /healthz on --health-addr (default TMS_WORKER_HEALTH_ADDR,
// 127.0.0.1:8081 — loopback only). `tms worker --health` is the probe
// mode used as the container healthcheck: it GETs that endpoint and exits
// 0/1. An HTTP probe was chosen over a health *file* because the runtime
// image is distroless (no shell for `test -f`) and file mtime freshness
// is awkward to verify from a probe subprocess; the probe runs inside the
// same container netns, so loopback is sufficient and nothing is exposed.
func newWorkerCmd(env *runtimeEnv) *cobra.Command {
	var (
		healthProbe       bool
		healthAddr        string
		concurrency       int
		pollInterval      time.Duration
		heartbeatInterval time.Duration
		staleAfter        time.Duration
		reapInterval      time.Duration
		drainTimeout      time.Duration
	)

	cmd := &cobra.Command{
		Use:   "worker",
		Short: "Run the background job worker (durable tms.jobs queue)",
		Long: "Runs a pool of concurrent job executors over the durable PostgreSQL\n" +
			"job queue (tms.jobs): SKIP LOCKED claiming, heartbeats with stale-claim\n" +
			"recovery, cooperative cancellation, per-job panic isolation and\n" +
			"graceful drain on SIGTERM. Job events stream to Redis channel\n" +
			"\"" + jobs.DefaultEventsChannel + "\" for the live UI.\n\n" +
			"With --health, instead probes a running worker's /healthz endpoint\n" +
			"and exits 0 (healthy) or 1 — used as the container healthcheck.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			addr := healthAddr
			if addr == "" {
				addr = env.cfg.WorkerHealthAddr
			}
			if healthProbe {
				return probeWorkerHealth(cmd.Context(), addr)
			}
			if concurrency == 0 {
				concurrency = env.cfg.WorkerConcurrency
			}
			return runWorker(cmd.Context(), env, addr, jobs.WorkerOptions{
				Concurrency:       concurrency,
				PollInterval:      pollInterval,
				HeartbeatInterval: heartbeatInterval,
				StaleAfter:        staleAfter,
				ReapInterval:      reapInterval,
				DrainTimeout:      drainTimeout,
			})
		},
	}

	cmd.Flags().BoolVar(&healthProbe, "health", false, "probe a running worker's /healthz and exit 0/1 (container healthcheck mode)")
	cmd.Flags().StringVar(&healthAddr, "health-addr", "", "liveness HTTP listen/probe address (default: TMS_WORKER_HEALTH_ADDR)")
	cmd.Flags().IntVar(&concurrency, "concurrency", 0, "parallel job executors (default: TMS_WORKER_CONCURRENCY)")
	cmd.Flags().DurationVar(&pollInterval, "poll-interval", jobs.DefaultPollInterval, "idle claim-poll cadence")
	cmd.Flags().DurationVar(&heartbeatInterval, "heartbeat-interval", jobs.DefaultHeartbeatInterval, "running-job heartbeat cadence (must be <= stale-after/3)")
	cmd.Flags().DurationVar(&staleAfter, "stale-after", jobs.DefaultStaleAfter, "heartbeat TTL after which dead workers' jobs are reclaimed")
	cmd.Flags().DurationVar(&reapInterval, "reap-interval", jobs.DefaultReapInterval, "stale-claim reaper cadence")
	cmd.Flags().DurationVar(&drainTimeout, "drain-timeout", jobs.DefaultDrainTimeout, "max wait for in-flight jobs on shutdown before they are released")
	return cmd
}

func runWorker(ctx context.Context, env *runtimeEnv, healthAddr string, opts jobs.WorkerOptions) error {
	log := env.log.With().Str("cmd", "worker").Logger()

	pool, err := db.NewPool(ctx, env.cfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	// Redis events are best-effort by design: an unreachable Redis demotes
	// the worker to queue-only operation (no live UI events) with a loud
	// warning instead of refusing to start — the durable queue is the
	// source of truth.
	var (
		queueOpts   []jobs.Option
		redisClient *redis.Client
	)
	redisClient = redis.NewClient(&redis.Options{
		Addr:     env.cfg.RedisAddr,
		DB:       env.cfg.RedisDB,
		Password: env.cfg.RedisPassword,
	})
	pingCtx, cancelPing := context.WithTimeout(ctx, 3*time.Second)
	if err := redisClient.Ping(pingCtx).Err(); err != nil {
		cancelPing()
		log.Warn().Err(err).Str("addr", env.cfg.RedisAddr).
			Msg("redis unreachable; job events disabled (queue unaffected)")
		_ = redisClient.Close()
		redisClient = nil
	} else {
		cancelPing()
		queueOpts = append(queueOpts, jobs.WithNotifier(
			jobs.NewRedisNotifier(redisClient, jobs.DefaultEventsChannel, log)))
	}
	defer func() {
		if redisClient != nil {
			_ = redisClient.Close()
		}
	}()

	queue, err := jobs.NewQueue(pool, log, queueOpts...)
	if err != nil {
		return err
	}

	// The NYSE calendar is shared by the universe rebuild handler and the
	// Sharadar API syncer (the latter normalizes every "today"/trading-date
	// to America/New_York per P1 locked decision 2).
	cal, err := calendar.NewNYSE()
	if err != nil {
		return err
	}

	registry := jobs.NewRegistry()
	// data.refresh: parquet backfill via the P0 importer plus the live
	// Nasdaq Data Link catchup engine for source=api. The API syncer is
	// built from config.NasdaqDataLinkAPIKey; when the key is absent the
	// worker still starts (parquet jobs unaffected) and source=api jobs
	// fail fast with a clear "key not set" message.
	apiSyncer, err := buildSharadarAPISyncer(pool, cal, env.cfg, log)
	if err != nil {
		return err
	}
	dataRefresh, err := handlers.NewDataRefresh(pool, log, env.cfg.SharadarCacheDir, apiSyncer)
	if err != nil {
		return err
	}
	if err := registry.Register(dataRefresh); err != nil {
		return err
	}

	// universe.rebuild: recompute the SEPA universe (NY as-of date, warmup
	// window, exclusions, market-cap cap, ranking) and append a
	// tms.universe_snapshots row. The API (POST /api/v1/universe/rebuild)
	// enqueues these; without this registration the worker would fail them
	// with "unknown kind".
	universeRebuild, err := handlers.NewUniverseRebuild(pool, cal, log)
	if err != nil {
		return err
	}
	if err := registry.Register(universeRebuild); err != nil {
		return err
	}

	// backtest.run: run a deterministic backtest through internal/engine,
	// persist the result to research.* (DB source of truth) and emit the
	// legacy runs/{ts}/*.json artifact set. The API (POST /api/v1/backtests)
	// enqueues these.
	backtest, err := handlers.NewBacktestWithParamsDir(pool, env.cfg.RunsDir, env.cfg.StrategyParamsDir, log)
	if err != nil {
		return err
	}
	if err := registry.Register(backtest); err != nil {
		return err
	}

	// hyperopt.run: run a full NSGA-II walk-forward hyperparameter study over a
	// shared read-only bar dataset, persisting trials to research.hyperopt_* and
	// emitting the runs/hyperopt/<ts>/ artifact tree. The API
	// (POST /api/v1/hyperopt) enqueues these.
	hyperoptRun, err := handlers.NewHyperopt(pool, env.cfg.RunsDir, env.cfg.StrategyParamsDir, log)
	if err != nil {
		return err
	}
	if err := registry.Register(hyperoptRun); err != nil {
		return err
	}

	// eod.refresh: idempotent EOD engine-replay (P5 decision 4). Replays
	// [as_of-window, as_of] bars through the SAME engine as backtest and UPSERTs
	// each strategy's evaluate_intent into tms.signal_intents idempotently on
	// (strategy_id, symbol, as_of) (a re-run overwrites, no dupes) + publishes to
	// Redis. Enqueued by the API / `tms eod --as-of <date> --enqueue`.
	eodRefresh, err := handlers.NewEODRefresh(pool, redisClient, env.cfg.StrategyParamsDir, log)
	if err != nil {
		return err
	}
	if err := registry.Register(eodRefresh); err != nil {
		return err
	}

	worker, err := jobs.NewWorker(queue, registry, log, opts)
	if err != nil {
		return err
	}

	healthSrv, err := startWorkerHealthServer(env, healthAddr, worker)
	if err != nil {
		return err
	}

	// Blocks until SIGINT/SIGTERM cancels ctx, then drains in-flight jobs
	// (bounded by DrainTimeout; overruns are released back to the queue).
	runErr := worker.Run(ctx)

	shutdownErr := app.GracefulShutdown(log, 10*time.Second,
		app.ShutdownFunc{Name: "health-server", Fn: healthSrv.Shutdown},
	)
	return errors.Join(runErr, shutdownErr)
}

// buildSharadarAPISyncer constructs the source=api catchup engine for
// data.refresh, or returns a genuine nil interface (not a typed nil) when
// no Nasdaq Data Link API key is configured — so DataRefresh's `api == nil`
// fast-fail for source=api fires correctly and the worker still starts to
// serve parquet refreshes and every other job kind.
//
// Returning the concrete *handlers.SharadarAPISyncer directly would smuggle
// a typed nil through the APISyncer interface and defeat that guard; hence
// the explicit interface return with an untyped nil on the no-key path.
func buildSharadarAPISyncer(pool *pgxpool.Pool, cal *calendar.Calendar, cfg *config.Config, log zerolog.Logger) (handlers.APISyncer, error) {
	key := strings.TrimSpace(cfg.NasdaqDataLinkAPIKey)
	if key == "" {
		log.Warn().Msg("TMS_NASDAQ_DATA_LINK_API_KEY not set; data.refresh source=api disabled " +
			"(parquet source and all other jobs unaffected)")
		return nil, nil
	}

	client, err := sharadar.NewClient(key, sharadar.WithLogger(log))
	if err != nil {
		return nil, fmt.Errorf("worker: building Nasdaq Data Link client: %w", err)
	}
	syncer, err := sharadar.NewSyncer(pool, client, cal, sharadar.WithSyncLogger(log))
	if err != nil {
		return nil, fmt.Errorf("worker: building Sharadar syncer: %w", err)
	}
	adapter, err := handlers.NewSharadarAPISyncer(syncer, log)
	if err != nil {
		return nil, err
	}
	log.Info().Msg("data.refresh source=api enabled (Nasdaq Data Link catchup engine)")
	return adapter, nil
}

// workerHealthServer wraps the liveness HTTP server so GracefulShutdown
// can drain it.
type workerHealthServer struct {
	srv *http.Server
}

func (h *workerHealthServer) Shutdown(ctx context.Context) error {
	if h.srv == nil {
		return nil
	}
	if err := h.srv.Shutdown(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// startWorkerHealthServer serves GET /healthz with worker liveness state
// on addr (loopback by default; the compose healthcheck probes from inside
// the same container netns).
func startWorkerHealthServer(env *runtimeEnv, addr string, worker *jobs.Worker) (*workerHealthServer, error) {
	log := env.log.With().Str("component", "worker-health").Logger()

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("worker: health listener on %s: %w", addr, err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		status := "ok"
		code := http.StatusOK
		if !worker.Started() {
			status, code = "starting", http.StatusServiceUnavailable
		}
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":    status,
			"worker_id": worker.ID(),
			"in_flight": worker.InFlight(),
		})
	})

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error().Err(err).Msg("health server stopped unexpectedly")
		}
	}()
	log.Info().Str("addr", ln.Addr().String()).Msg("worker health endpoint listening")
	return &workerHealthServer{srv: srv}, nil
}

// probeWorkerHealth is the --health container-healthcheck mode: GET
// /healthz on addr with a short deadline, exit 0 on HTTP 200.
func probeWorkerHealth(ctx context.Context, addr string) error {
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, "http://"+addr+"/healthz", nil)
	if err != nil {
		return fmt.Errorf("worker health probe: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("worker health probe: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("worker health probe: %s returned %s", addr, resp.Status)
	}
	return nil
}
