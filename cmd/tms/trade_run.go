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

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/spf13/cobra"

	"github.com/byjackchen/trade-tms-go/internal/api"
	"github.com/byjackchen/trade-tms-go/internal/apistore"
	"github.com/byjackchen/trade-tms-go/internal/app"
	"github.com/byjackchen/trade-tms-go/internal/broker/moomoo"
	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/db"
	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/preflight"
	"github.com/byjackchen/trade-tms-go/internal/runner"
)

// resolveRun validates the operator-facing run selector — the 2D model
// (docs/concept-alignment.md §1.3): an ExecutionPolicy (signal vs auto) plus a
// bound account env (simu/paper/real). It returns the parsed axes and the
// DERIVED convenience word (signal|paper|live) the runtime plumbing still uses
// internally (runner.Live's restart vocabulary). exec=auto requires an env;
// exec=signal needs none, but MAY carry --env paper|real to designate a broker
// account to SYNC READ-ONLY from (DIRECTION 2 — pull hand-placed positions back);
// signal with no env (or --env simu) keeps the env empty (pure emit-only, no sync).
//
// "go paper" = exec=auto on a simu/paper account; "go live" = exec=auto on a
// real account; "signal" = exec=signal. Real money (env=real) is gated upstream.
func resolveRun(execStr, envStr string) (domain.ExecutionPolicy, domain.BrokerEnv, string, error) {
	exec, err := domain.ParseExecutionPolicy(execStr)
	if err != nil {
		return "", "", "", fmt.Errorf("--exec-policy %q invalid (want signal|auto)", execStr)
	}
	if exec == domain.ExecSignal {
		// signal emits only — no auto orders, so the run word stays "signal". BUT
		// --env may still designate a broker account to SYNC READ-ONLY from
		// (DIRECTION 2): the operator hand-trades at the broker, then pulls those
		// positions back into the EXTERNAL book. No --env (or --env simu = the
		// synthetic no-broker account) means pure signal with no sync account.
		env := domain.BrokerEnv(strings.TrimSpace(envStr))
		if env == "" || env == domain.EnvSimu {
			return exec, "", domain.RunWord(exec, ""), nil
		}
		if !env.IsValid() {
			return "", "", "", fmt.Errorf("--env %q invalid (want simu|paper|real)", envStr)
		}
		return exec, env, domain.RunWord(exec, ""), nil
	}
	env := domain.BrokerEnv(strings.TrimSpace(envStr))
	if env == "" {
		return "", "", "", fmt.Errorf("--exec-policy auto requires --env (simu|paper|real): a 'go paper' is auto on a paper account, 'go live' is auto on a real account")
	}
	if !env.IsValid() {
		return "", "", "", fmt.Errorf("--env %q invalid (want simu|paper|real)", envStr)
	}
	return exec, env, domain.RunWord(exec, env), nil
}

// newTradeRunCmd implements `tms trade run --mode signal`: the live (real-time)
// trading node. It wires the native moomoo OpenD client (or the protocol-faithful
// mock, switched by TMS_MOOMOO_ADDR) -> a streaming feed -> the SAME internal/core
// engine + strategy / portfolio / warmup code as backtest, driven by a wall
// clock, recording a Signal per strategy per bar (tms.signals +
// Redis streams) and submitting NO orders — all under the ops.commands control
// plane (halt/resume/kill/stop/set_mode) with full audit and a graceful
// lifecycle (ctx cancellation, drain, no goroutine leaks, structured logs, no
// secrets logged).
//
// Only --mode signal is accepted; paper/live (order submission, fills, FLATTEN)
// are deferred to P6 (locked decision 1 + 6).
func newTradeRunCmd(env *runtimeEnv) *cobra.Command {
	var (
		execPolicy    string
		envStr        string
		account       string
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
	)

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run the live trading node (signal mode; paper/live deferred to P6)",
		Long: "Runs the live (real-time) engine: the SAME internal/core event loop and\n" +
			"strategy / portfolio / warmup code as backtest, driven by a wall clock and a\n" +
			"streaming moomoo (or mock OpenD) bar feed. In signal mode it records a\n" +
			"Signal per strategy per bar (tms.signals + Redis streams) and\n" +
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
				execPolicy:    strings.TrimSpace(execPolicy),
				env:           strings.TrimSpace(envStr),
				account:       strings.TrimSpace(account),
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
			})
		},
	}

	cmd.Flags().StringVar(&execPolicy, "exec-policy", "signal", "execution policy: signal (emit-only) | auto (auto-submit). 'go paper' = auto + --env paper; 'go live' = auto + --env real")
	cmd.Flags().StringVar(&envStr, "env", "", "account env: simu | paper | real. Required for --exec-policy auto (auto-submit). For --exec-policy signal it is OPTIONAL — paper|real designates a broker account to SYNC read-only from (real requires real acc id + TMS_LIVE_CONFIRM + the TMS-LIVE-REAL-001 trader id)")
	cmd.Flags().StringVar(&account, "account", "", "explicit account id (tms.accounts) to bind; default = the (moomoo, env) default account. Accounts are managed in the UI — no longer from .env")
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
	cmd.Flags().BoolVar(&skipPreflight, "skip-preflight", false, "DANGER: start without the go-live preflight gate (signal/paper only; REFUSED for live = exec-policy auto + env real)")
	cmd.Flags().IntVar(&maxStaleDays, "max-stale-days", 1, "DATA_CURRENT tolerance: max trading days the data frontier may lag T-1")

	return cmd
}

// newTradePreflightCmd implements `tms trade preflight`: run the go-live precondition
// checks for a session (mode/strategy/tickers) and print a PASS/FAIL table. Exits 0
// when all BLOCKER checks pass, 1 otherwise — the machine-enforceable go/no-go gate
// the operator (and CI) can run before a paper/live session.
func newTradePreflightCmd(env *runtimeEnv) *cobra.Command {
	var (
		execPolicy   string
		envStr       string
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
			"Exit 0 when every BLOCKER passes, 1 otherwise. exec-policy signal treats data\n" +
			"freshness + OpenD as advisory (warn); auto (paper/live) requires all blockers.\n" +
			"This is the same report 'tms trade run' enforces at startup and GET /api/v1/trade/preflight serves.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runTradePreflight(cmd.Context(), env, preflightArgs{
				execPolicy:   strings.TrimSpace(execPolicy),
				env:          strings.TrimSpace(envStr),
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
	cmd.Flags().StringVar(&execPolicy, "exec-policy", "signal", "execution policy: signal (emit-only) | auto (auto-submit; paper/live)")
	cmd.Flags().StringVar(&envStr, "env", "", "bound account env for --exec-policy auto: simu | paper | real")
	cmd.Flags().StringVar(&strategy, "strategy", "multi", "strategy: sepa | sector_rotation | pairs | orb | multi")
	cmd.Flags().StringVar(&tickersCSV, "tickers", "", "comma-separated SEPA stock universe (sepa/multi); empty resolves the default SF1 window universe")
	cmd.Flags().StringVar(&orbSymbol, "orb-symbol", "", "ORB strategy: the single intraday instrument symbol")
	cmd.Flags().StringVar(&moomooAddr, "moomoo-addr", "", "moomoo OpenD address for the OpenD probe (default TMS_MOOMOO_ADDR)")
	cmd.Flags().IntVar(&maxStaleDays, "max-stale-days", 1, "DATA_CURRENT tolerance: max trading days the data frontier may lag T-1")
	cmd.Flags().BoolVar(&checkOpenD, "check-opend", false, "probe OpenD even in signal mode (auto always probes it)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the report as JSON instead of the table")
	return cmd
}

// resolveBoundAccount loads the broker account a run binds from tms.accounts — by
// explicit id (--account) when given, else the (moomoo, env) default. Accounts are
// USER-MANAGED in the UI; .env no longer carries account ids. Returns an actionable
// error when no account / no default exists for the env.
func resolveBoundAccount(ctx context.Context, pool *pgxpool.Pool, accountID string, env domain.BrokerEnv) (domain.Account, error) {
	store := apistore.NewTradeStore(pool)
	var (
		info api.TradeAccountInfo
		err  error
	)
	if id := strings.TrimSpace(accountID); id != "" {
		if info, err = store.GetAccount(ctx, id); err != nil {
			return domain.Account{}, fmt.Errorf("--account %q not found in tms.accounts (create it in the UI): %w", id, err)
		}
	} else if info, err = store.DefaultAccount(ctx, "moomoo", env); err != nil {
		return domain.Account{}, fmt.Errorf("no default %s account in tms.accounts — create one and mark it default in the UI, or pass --account: %w", env, err)
	}
	if domain.BrokerEnv(info.Env) != env {
		return domain.Account{}, fmt.Errorf("account %s env is %q but this run needs %q", info.ID, info.Env, env)
	}
	return domain.Account{
		ID:          info.ID,
		Venue:       info.Venue,
		Env:         domain.BrokerEnv(info.Env),
		BrokerAccID: uint64(info.BrokerAccID),
		Label:       info.Label,
		IsDefault:   info.IsDefault,
		Notes:       info.Notes,
	}, nil
}

type tradeRunArgs struct {
	execPolicy    string
	env           string
	account       string
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
}

// runTradeRun assembles and runs the live signal-mode trading node (P5): the native
// moomoo OpenD client (or mock by TMS_MOOMOO_ADDR) -> a streaming feed -> the
// same internal/core engine + strategy/portfolio/warmup as backtest, driven by
// a wall clock, recording a signal per strategy per bar to PG + Redis and
// submitting NO orders, under the ops.commands control plane.
func runTradeRun(parent context.Context, env *runtimeEnv, a tradeRunArgs) error {
	execPolicy, acctEnv, mode, err := resolveRun(a.execPolicy, a.env)
	if err != nil {
		return err
	}

	log := env.log.With().
		Str("cmd", "trade run").
		Str("exec_policy", string(execPolicy)).
		Str("env", string(acctEnv)).
		Str("run", mode).
		Str("trader_id", a.traderID).
		Str("strategy", a.strategy).
		Logger()

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
	// HARD GUARD (finding 1): --skip-preflight is NEVER honored for a real-money
	// run (exec-policy auto + env real). Allowing an operator to start REAL-MONEY
	// trading with zero precondition verification is exactly the bypassable-blocker
	// the preflight exists to close. For live the preflight is mandatory and
	// non-overridable; signal/paper may still skip it (loudly).
	isLiveRun := acctEnv == domain.EnvReal
	if a.skipPreflight && isLiveRun {
		return fmt.Errorf("--skip-preflight is refused for a live run (exec-policy auto + env real): the go-live preflight is MANDATORY for real-money trading and cannot be bypassed (run `tms trade preflight --exec-policy auto --env real ...` to see the failing blockers and resolve them)")
	}
	if a.skipPreflight {
		log.Warn().Str("run", mode).Msg("PREFLIGHT SKIPPED (--skip-preflight): starting WITHOUT go-live precondition checks — blockers are NOT enforced (NOT permitted for a live run)")
	} else {
		rep := preflight.Run(ctx, preflight.Config{
			ExecPolicy:          execPolicy,
			Env:                 acctEnv,
			Strategy:            a.strategy,
			Tickers:             tickers,
			ORBSymbol:           a.orbSymbol,
			MaxStaleTradingDays: a.maxStaleDays,
			// auto (paper/live) always probes OpenD; in signal we do too here (the node
			// is about to open a live OpenD socket regardless, so an unreachable broker
			// is worth surfacing — as a warn for signal, a blocker for auto).
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
			// --skip-preflight is offered as an override ONLY for signal/paper; for
			// a live run it is refused above, so do not advertise it as an option.
			if isLiveRun {
				return fmt.Errorf("go-live preflight failed: %d blocker(s) must be resolved before real-money (live) trading", len(rep.Blockers()))
			}
			return fmt.Errorf("go-live preflight failed: %d blocker(s) must be resolved (or pass --skip-preflight to override for signal/paper)", len(rep.Blockers()))
		}
		log.Info().Int("checks", len(rep.Checks)).Int("warnings", len(rep.Warnings())).Msg("go-live preflight passed")
	}

	// The broker account is RESOLVED FROM tms.accounts (user-managed in the UI) — by
	// --account id, else the (moomoo, env) default — NOT from .env. Needed when the
	// run designates a broker env (auto paper/live, or signal + --env paper|real for
	// read-only sync). Pure signal binds the synthetic simu account, no broker.
	var bound domain.Account
	if acctEnv == domain.EnvPaper || acctEnv == domain.EnvReal {
		bound, err = resolveBoundAccount(ctx, pool, a.account, acctEnv)
		if err != nil {
			return err
		}
	}
	reconcileEvery := parseDurationEnv("TMS_RECONCILE_INTERVAL", 5*time.Minute)

	node, err := runner.NewLive(pool, redisClient, runner.LiveConfig{
		TraderID:               a.traderID,
		Mode:                   mode,
		Strategy:               a.strategy,
		Tickers:                tickers,
		ORBSymbol:              a.orbSymbol,
		StartingBalance:        a.startBalance,
		MoomooAddr:             moomooAddr,
		MoomooMaxSub:           env.cfg.MoomooMaxSub,
		BarSeconds:             a.barSeconds,
		ParamsDir:              env.cfg.StrategyParamsDir,
		DrainTimeout:           a.drainTimeout,
		BoundAccount:           bound,
		UnlockPassword:         strings.TrimSpace(os.Getenv("TMS_MOOMOO_UNLOCK_PASSWORD")),
		LiveConfirmationPhrase: strings.TrimSpace(os.Getenv("TMS_LIVE_CONFIRM")),
		ReconcileInterval:      reconcileEvery,
	}, log)
	if err != nil {
		return err
	}

	// Liveness HTTP server (compose healthcheck on :18090 via host port). It ALSO
	// serves the READ-ONLY broker-SYNC surface (/api/v1/trade/sync,/status,/account)
	// off the same listener, bearer-guarded by TMS_API_TOKEN — the trade node holds
	// the broker connection, so there is no separate manual listener / reverse proxy.
	healthSrv, err := startTradeHealthServer(a.healthAddr, node, env.cfg.APIToken, log)
	if err != nil {
		return err
	}

	// DIRECTION 2 (broker -> TMS): connect the READ-ONLY broker-sync desk to the
	// account designated by --env (simulate -> the paper account, real -> the real
	// account), INDEPENDENT of exec_policy. This makes "Sync from broker" work in
	// SIGNAL mode too — the operator hand-trades AT the broker, then pulls those
	// externally-placed positions back into the EXTERNAL book + reconciles — not just
	// in auto. Signal with no --env (or --env simu) binds no broker account, so there
	// is nothing to sync and /trade/sync stays 503. The sync is read-only (Trd_Get*
	// only); a REAL account still passes the full live gate in ConnectBrokerSync.
	var syncMode string
	switch acctEnv {
	case domain.EnvPaper:
		syncMode = "paper"
	case domain.EnvReal:
		syncMode = "live"
	}
	if syncMode != "" {
		go connectBrokerSyncDesk(ctx, node, syncMode, log)
	}

	log.Info().
		Str("moomoo_addr", moomooAddr).
		Str("run", mode).
		Float64("health_nav", a.startBalance).
		Msg("live node starting")

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

// preflightArgs carries the `tms trade preflight` flags.
type preflightArgs struct {
	execPolicy   string
	env          string
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
	execPolicy, acctEnv, runWord, err := resolveRun(a.execPolicy, a.env)
	if err != nil {
		return err
	}
	log := env.log.With().Str("cmd", "trade preflight").
		Str("exec_policy", string(execPolicy)).Str("env", string(acctEnv)).Str("run", runWord).
		Str("strategy", a.strategy).Logger()

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
		ExecPolicy:          execPolicy,
		Env:                 acctEnv,
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
// on addr (the compose healthcheck targets the internal :18090 via host port). It
// ALSO folds the READ-ONLY broker-SYNC surface (DIRECTION 2: /api/v1/trade/sync,
// /status, /account) onto the same listener, bearer-guarded by syncToken — the
// trade node holds the broker connection, so there is no separate manual listener
// and no reverse proxy. An empty syncToken leaves the sync routes unmounted.
func startTradeHealthServer(addr string, node *runner.Live, syncToken string, log zerolog.Logger) (*tradeHealthServer, error) {
	hlog := log.With().Str("component", "trade-health").Logger()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("trade: health listener on %s: %w", addr, err)
	}
	mux := http.NewServeMux()
	// DIRECTION 2 (broker -> TMS): READ-ONLY sync surface, bearer-guarded.
	registerBrokerSyncRoutes(mux, node, syncToken, hlog)
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
