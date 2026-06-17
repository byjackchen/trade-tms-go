package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/spf13/cobra"

	"github.com/byjackchen/trade-tms-go/internal/app"
	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/db"
	"github.com/byjackchen/trade-tms-go/internal/jobs"
	"github.com/byjackchen/trade-tms-go/internal/scheduler"
)

// newSchedulerCmd implements `tms scheduler`: the NYSE-calendar-aware daily
// incremental-sync scheduler. On every trading day, at TMS_SCHEDULER_DAILY_AT
// (interpreted in TMS_SCHEDULER_TZ — default 18:30 America/New_York, a few
// hours after the 16:00 ET close when Sharadar has published the session EOD)
// it ENQUEUES the daily pipeline onto the durable tms.jobs queue:
// data.refresh source=api (incremental Sharadar catchup) then eod.refresh
// (signal-intent precompute). Idempotent + single-leader via the
// tms.scheduler_runs ledger (exactly one pipeline per trading day even across
// restarts / multiple instances); weekends and holidays are skipped via
// internal/data/calendar.
//
// Health model (same pattern as `tms worker`/`tms api`): the runtime image is
// distroless, so the compose healthcheck execs `tms scheduler --health`,
// which GETs the scheduler's own loopback /healthz and exits 0/1.
func newSchedulerCmd(env *runtimeEnv) *cobra.Command {
	var (
		healthProbe bool
		healthAddr  string
		dailyAt     string
		tz          string
		catchup     bool
		tick        time.Duration
	)

	cmd := &cobra.Command{
		Use:   "scheduler",
		Short: "Run the daily incremental-sync scheduler (NYSE-calendar-aware)",
		Long: "Enqueues the daily data pipeline (data.refresh source=api then\n" +
			"eod.refresh) onto the durable tms.jobs queue on every NYSE trading day\n" +
			"at the configured time after the US close (TMS_SCHEDULER_DAILY_AT in\n" +
			"TMS_SCHEDULER_TZ; default 18:30 America/New_York). Idempotent and\n" +
			"single-leader via the tms.scheduler_runs ledger — exactly one pipeline\n" +
			"per trading day even across restarts or multiple instances. Weekends\n" +
			"and holidays are skipped (internal/data/calendar). With startup\n" +
			"catch-up (default on) a restart after the fire time still enqueues the\n" +
			"day once.\n\n" +
			"With --health, instead probes a running scheduler's /healthz endpoint\n" +
			"and exits 0 (healthy) or 1 — used as the container healthcheck.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			addr := healthAddr
			if addr == "" {
				addr = env.cfg.SchedulerHealthAddr
			}
			if healthProbe {
				return probeSchedulerHealth(cmd.Context(), addr)
			}
			at := dailyAt
			if at == "" {
				at = env.cfg.SchedulerDailyAt
			}
			zone := tz
			if zone == "" {
				zone = env.cfg.SchedulerTZ
			}
			// --catchup overrides config only when explicitly set on the CLI.
			useCatchup := env.cfg.SchedulerCatchup
			if cmd.Flags().Changed("catchup") {
				useCatchup = catchup
			}
			return runScheduler(cmd.Context(), env, addr, at, zone, useCatchup, tick)
		},
	}

	cmd.Flags().BoolVar(&healthProbe, "health", false, "probe a running scheduler's /healthz and exit 0/1 (container healthcheck mode)")
	cmd.Flags().StringVar(&healthAddr, "health-addr", "", "liveness HTTP listen/probe address (default: TMS_SCHEDULER_HEALTH_ADDR)")
	cmd.Flags().StringVar(&dailyAt, "daily-at", "", "daily fire time HH:MM (default: TMS_SCHEDULER_DAILY_AT)")
	cmd.Flags().StringVar(&tz, "tz", "", "IANA time zone for --daily-at (default: TMS_SCHEDULER_TZ)")
	cmd.Flags().BoolVar(&catchup, "catchup", true, "on startup, enqueue today's pipeline if its fire time already passed (default: TMS_SCHEDULER_CATCHUP)")
	cmd.Flags().DurationVar(&tick, "tick", scheduler.DefaultTick, "loop re-evaluation cadence")
	return cmd
}

func runScheduler(ctx context.Context, env *runtimeEnv, healthAddr, dailyAt, tz string, catchup bool, tick time.Duration) error {
	log := env.log.With().Str("cmd", "scheduler").Logger()

	at, err := scheduler.ParseTimeOfDay(dailyAt)
	if err != nil {
		return err
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return fmt.Errorf("scheduler: invalid TMS_SCHEDULER_TZ %q: %w", tz, err)
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

	// Redis events are best-effort (same contract as the worker/api): an
	// unreachable Redis demotes the scheduler's own enqueue notifications to
	// silent (the durable queue + ledger are the source of truth).
	redisClient := redis.NewClient(&redis.Options{
		Addr:     env.cfg.RedisAddr,
		DB:       env.cfg.RedisDB,
		Password: env.cfg.RedisPassword,
	})
	defer func() { _ = redisClient.Close() }()
	var queueOpts []jobs.Option
	pingCtx, cancelPing := context.WithTimeout(ctx, 3*time.Second)
	if perr := redisClient.Ping(pingCtx).Err(); perr != nil {
		log.Warn().Err(perr).Str("addr", env.cfg.RedisAddr).
			Msg("redis unreachable; scheduler enqueue events disabled (queue + ledger unaffected)")
	} else {
		queueOpts = append(queueOpts, jobs.WithNotifier(
			jobs.NewRedisNotifier(redisClient, jobs.DefaultEventsChannel, log)))
	}
	cancelPing()

	queue, err := jobs.NewQueue(pool, log, queueOpts...)
	if err != nil {
		return err
	}
	ledger, err := scheduler.NewPGLedger(pool)
	if err != nil {
		return err
	}

	instance := schedulerInstanceID()
	sched, err := scheduler.New(cal, queue, ledger, log, scheduler.Options{
		DailyAt:    at,
		Loc:        loc,
		Catchup:    catchup,
		Tick:       tick,
		InstanceID: instance,
	})
	if err != nil {
		return err
	}

	healthSrv, err := startSchedulerHealthServer(env, healthAddr, instance)
	if err != nil {
		return err
	}

	// Note (compose): the NASDAQ_DATA_LINK_API_KEY does NOT need to reach the
	// scheduler — it only ENQUEUES data.refresh source=api jobs; the WORKER
	// (which holds the key, cmd/tms/worker.go) runs the actual Nasdaq Data
	// Link sync. The key is documented on the scheduler service so the
	// catch-up/manual paths surface a clear error if a future inline mode is
	// added.

	runErr := sched.Run(ctx)

	shutdownErr := app.GracefulShutdown(log, 10*time.Second,
		app.ShutdownFunc{Name: "health-server", Fn: healthSrv.Shutdown},
	)
	return errors.Join(runErr, shutdownErr)
}

// schedulerInstanceID identifies this scheduler process in the ledger's
// claimed_by column (host:pid) for operator forensics across instances.
func schedulerInstanceID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "scheduler"
	}
	return host + ":" + strconv.Itoa(os.Getpid())
}

// schedulerHealthServer wraps the liveness HTTP server for GracefulShutdown.
type schedulerHealthServer struct{ srv *http.Server }

func (h *schedulerHealthServer) Shutdown(ctx context.Context) error {
	if h.srv == nil {
		return nil
	}
	if err := h.srv.Shutdown(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// startSchedulerHealthServer serves GET /healthz on addr (loopback by
// default; the compose healthcheck probes from inside the same container
// netns).
func startSchedulerHealthServer(env *runtimeEnv, addr, instance string) (*schedulerHealthServer, error) {
	log := env.log.With().Str("component", "scheduler-health").Logger()

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("scheduler: health listener on %s: %w", addr, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "instance": instance})
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error().Err(err).Msg("scheduler health server stopped unexpectedly")
		}
	}()
	log.Info().Str("addr", ln.Addr().String()).Msg("scheduler health endpoint listening")
	return &schedulerHealthServer{srv: srv}, nil
}

// probeSchedulerHealth is the --health container-healthcheck mode.
func probeSchedulerHealth(ctx context.Context, addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("scheduler health probe: invalid address %q: %w", addr, err)
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
		return fmt.Errorf("scheduler health probe: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("scheduler health probe: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("scheduler health probe: %s returned %s", url, resp.Status)
	}
	return nil
}
