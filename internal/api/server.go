package api

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog"

	"github.com/byjackchen/trade-tms-go/internal/app"
	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
)

// depPingTimeout bounds each /healthz dependency probe.
const depPingTimeout = 2 * time.Second

// PingFunc probes one dependency (PG, Redis); nil error = reachable.
type PingFunc func(ctx context.Context) error

// Deps wires a Server. Token, Jobs, Data, Universe, Calendar and PingPG
// are required; PingRedis may be nil (Redis-less deployment: /healthz
// reports the dependency as unavailable and the WS bridge is not running).
type Deps struct {
	Log         zerolog.Logger
	Token       string
	CORSOrigins []string
	Jobs        JobQueue
	Data        DataStore
	Universe    UniverseReader
	Runs        RunsReader
	Strategies  StrategyReader
	Hyperopt    HyperoptReader
	Promoter    HyperoptPromoter
	// Compositions backs the /api/v1/compositions CRUD + the composition_id
	// resolution the backtest and optimize endpoints do. Optional: when nil those
	// endpoints return 503.
	Compositions CompositionStore
	// AuditLog appends rows to tms.audit_log for the Composition mutation endpoints.
	// Optional: when nil those mutations skip the audit write (best-effort).
	AuditLog  AuditWriter
	Calendar  *calendar.Calendar
	PingPG    PingFunc
	PingRedis PingFunc
	// Trade is the trade cockpit read surface (PG-backed). Optional: when nil the
	// /api/v1/trade/* read endpoints return 503.
	Trade TradeReader
	// Commands enqueues audited control commands. Optional: when nil
	// POST /api/v1/trade/commands returns 503.
	Commands CommandEnqueuer
	// System supplies the aggregate counts + freshness for GET /api/v1/system.
	// Optional: when nil those components report "not_configured" (the endpoint
	// still serves PG/Redis/feed status).
	System SystemReader
	// Audit reads the append-only tms.audit_log for GET /api/v1/audit. Optional:
	// when nil that endpoint returns 503 (audit reader not configured).
	Audit AuditReader
	// Sync forces the daily incremental-sync pipeline (POST
	// /api/v1/data/sync-now). Optional: when nil that endpoint returns 503.
	Sync SyncForcer
	// Preflight runs the go-live preflight for GET /api/v1/trade/preflight.
	// Optional: when nil that endpoint returns 503 (preflight not wired).
	Preflight PreflightRunner
	// BrokerSync is the READ-ONLY broker-sync surface (DIRECTION 2: pull the
	// externally-placed broker state into TMS under the EXTERNAL book + reconcile).
	// Optional: when nil the /api/v1/trade/sync + /trade/status endpoints return 503
	// (no broker-connected session). It places NO orders — safe in ALL modes incl
	// signal.
	BrokerSync BrokerSync
	// Now overrides the clock (tests); nil = time.Now.
	Now func() time.Time
}

// Server is the HTTP/WebSocket API for the UI (contract: docs/api.md).
type Server struct {
	log          zerolog.Logger
	token        string
	corsOrigins  []string
	jobs         JobQueue
	data         DataStore
	uni          UniverseReader
	runs         RunsReader
	strat        StrategyReader
	hyperopt     HyperoptReader
	promoter     HyperoptPromoter
	compositions CompositionStore
	auditLog     AuditWriter
	cal          *calendar.Calendar
	pingPG       PingFunc
	pingRedis    PingFunc
	trade        TradeReader
	commands     CommandEnqueuer
	system       SystemReader
	audit        AuditReader
	sync         SyncForcer
	preflight    PreflightRunner
	brokerSync   BrokerSync
	hub          *Hub
	now          func() time.Time
}

// NewServer validates deps and builds the server (including its WS hub).
func NewServer(d Deps) (*Server, error) {
	switch {
	case d.Token == "":
		return nil, errors.New("api: empty bearer token (set TMS_API_TOKEN)")
	case d.Jobs == nil:
		return nil, errors.New("api: nil job queue")
	case d.Data == nil:
		return nil, errors.New("api: nil data store")
	case d.Universe == nil:
		return nil, errors.New("api: nil universe reader")
	case d.Runs == nil:
		return nil, errors.New("api: nil runs reader")
	case d.Calendar == nil:
		return nil, errors.New("api: nil trading calendar")
	case d.PingPG == nil:
		return nil, errors.New("api: nil postgres ping")
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}
	log := d.Log.With().Str("component", "api").Logger()
	return &Server{
		log:          log,
		token:        d.Token,
		corsOrigins:  d.CORSOrigins,
		jobs:         d.Jobs,
		data:         d.Data,
		uni:          d.Universe,
		runs:         d.Runs,
		strat:        d.Strategies,
		hyperopt:     d.Hyperopt,
		promoter:     d.Promoter,
		compositions: d.Compositions,
		auditLog:     d.AuditLog,
		cal:          d.Calendar,
		pingPG:       d.PingPG,
		pingRedis:    d.PingRedis,
		trade:        d.Trade,
		commands:     d.Commands,
		system:       d.System,
		audit:        d.Audit,
		sync:         d.Sync,
		preflight:    d.Preflight,
		brokerSync:   d.BrokerSync,
		hub:          NewHub(log, d.CORSOrigins),
		now:          now,
	}, nil
}

// Hub exposes the WebSocket hub so the caller can attach the Redis event
// bridge and close it on shutdown.
func (s *Server) Hub() *Hub { return s.hub }

// Routes builds the chi router: public /healthz + /version, bearer-token
// guarded /api/v1/* (REST under a 60 s timeout; the WebSocket endpoint is
// mounted outside it — its lifetime is the connection's).
func (s *Server) Routes() *chi.Mux {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(requestLogger(s.log))
	r.Use(recoverer(s.log))
	r.Use(corsMiddleware(s.corsOrigins))

	r.Get("/healthz", s.handleHealthz)
	r.Get("/version", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{
			"version":    app.Version,
			"commit":     app.Commit,
			"build_date": app.BuildDate,
		})
	})

	r.Route("/api/v1", func(r chi.Router) {
		r.Use(requireAuth(s.token, s.log))

		// WebSocket: no request timeout (long-lived connection).
		r.Get("/ws", s.handleWS)

		r.Group(func(r chi.Router) {
			r.Use(middleware.Timeout(60 * time.Second))

			r.Get("/data/coverage", s.handleCoverage)
			r.Get("/data/tickers", s.handleTickerSearch)
			r.Get("/data/sync-runs", s.handleSyncRuns)
			r.Post("/data/refresh", s.handleDataRefresh)
			r.Post("/data/sync-now", s.handleSyncNow)

			r.Get("/jobs", s.handleJobList)
			r.Get("/jobs/{id}", s.handleJobGet)
			r.Post("/jobs/{id}/cancel", s.handleJobCancel)
			r.Post("/jobs/{id}/retry", s.handleJobRetry)

			// Append-only operational audit trail (Ops UI AUDIT LOG panel).
			r.Get("/audit", s.handleAuditList)

			r.Get("/universe/latest", s.handleUniverseLatest)
			r.Post("/universe/rebuild", s.handleUniverseRebuild)

			r.Get("/strategies", s.handleStrategyList)
			r.Get("/strategies/{id}", s.handleStrategyGet)

			// Compositions (named portfolio blueprints): CRUD only. The mutating
			// routes are audited (tms.audit_log). A Composition COMPOSES already-tuned
			// strategies + weights + risk and is VALIDATED by Backtest; it never
			// re-tunes params (params are tuned per-strategy in the Strategies module's
			// Hyperopt). Backtest drops in the Composition by id
			// (docs/concept-alignment.md §3.3).
			r.Get("/compositions", s.handleCompositionList)
			r.Get("/compositions/{id}", s.handleCompositionGet)
			r.Post("/compositions", s.handleCompositionCreate)
			r.Put("/compositions/{id}", s.handleCompositionUpdate)
			r.Delete("/compositions/{id}", s.handleCompositionDelete)
			// Composition-level hyperopt (kind=composition): tune a Composition's
			// member weights + cash + composite risk while every member's SIGNAL
			// params stay FIXED (decision 4). The study/trials are read through the
			// existing GET /hyperopt[/{id}/trials] (they carry kind=composition);
			// promote OVERWRITES the composition IN PLACE (decision 3). Per-strategy
			// SIGNAL-param tuning still lives at POST /hyperopt below.
			r.Post("/compositions/{id}/hyperopt", s.handleCompositionHyperoptEnqueue)
			r.Post("/compositions/{id}/hyperopt/{study_ts}/promote", s.handleCompositionHyperoptPromote)

			// Aggregated system status (P7 capstone): pg + redis + moomoo feed
			// + active sessions + job-queue depth + data freshness in one call
			// for the UI System page.
			r.Get("/system", s.handleSystem)

			r.Post("/backtests", s.handleBacktestEnqueue)
			r.Get("/backtests", s.handleBacktestList)
			r.Get("/backtests/{id}", s.handleBacktestGet)
			r.Get("/backtests/{id}/equity", s.handleBacktestEquity)
			r.Get("/backtests/{id}/trades", s.handleBacktestTrades)
			r.Get("/backtests/{id}/orders", s.handleBacktestOrders)

			r.Post("/hyperopt", s.handleHyperoptEnqueue)
			r.Get("/hyperopt", s.handleHyperoptList)
			r.Get("/hyperopt/{id}", s.handleHyperoptGet)
			r.Get("/hyperopt/{id}/trials", s.handleHyperoptTrials)
			r.Post("/hyperopt/{id}/promote", s.handleHyperoptPromote)

			// Trade cockpit (P5): read surface from PG + the audited command
			// enqueue endpoint. The trading mutation surface stays out of the
			// HTTP API (read-only forever); commands are the audited side channel.
			r.Get("/trade/session", s.handleTradeSession)
			r.Get("/trade/signals", s.handleTradeSignals)
			r.Get("/trade/health", s.handleTradeHealth)
			r.Get("/trade/preflight", s.handleTradePreflight)
			r.Get("/watchlist", s.handleWatchlist)
			// Paper/live trading read surface (P6 task 6). NOTE: /trade/account is
			// served by the broker-sync block below alongside the sync/status routes;
			// registering it here too would collide.
			r.Get("/trade/orders", s.handleTradeOrders)
			r.Get("/trade/fills", s.handleTradeFills)
			r.Get("/trade/positions", s.handleTradePositions)
			r.Get("/trade/reconciliation", s.handleTradeReconciliation)
			// Portfolio (the Account's runtime ledger, docs/concept-alignment.md
			// §3.3): one read aggregating {account snapshot, positions, health}.
			r.Get("/trade/portfolio", s.handleTradePortfolio)
			// Account registry (P5 step A): list accounts for the UI selector /
			// per-account filter. NOTE: distinct from /trade/account (the funds
			// snapshot served by the mutation block below).
			r.Get("/trade/accounts", s.handleTradeAccounts)
			r.Post("/trade/commands", s.handleTradeCommand)

			// Back-compat: the old /live/* read/control paths 301-redirect to
			// their /trade/* equivalents so a not-yet-updated UI keeps working.
			// (The /trade/* mutation surface below is unrelated and never had a
			// /live/* alias.) Most are the SAME path with the /live prefix swapped
			// for /trade; the legacy /live/intents redirects to the renamed
			// /trade/signals (concept-A intent->signal rename).
			for old, target := range map[string]string{
				"/live/intents": "/api/v1/trade/signals",
			} {
				r.Handle(old, redirectTo(target))
			}
			for _, suffix := range []string{
				"session", "health", "preflight",
				"orders", "fills", "positions", "account", "accounts", "reconciliation",
				"commands",
			} {
				old := "/live/" + suffix
				r.Handle(old, redirectTo("/api/v1/trade/"+suffix))
			}

			// Broker-SYNC surface (DIRECTION 2: broker -> TMS). READ-ONLY at the
			// broker (only Trd_Get* reads; it places NO orders) and safe in ALL
			// session modes incl signal. TMS no longer offers an order ticket — the
			// operator places orders at the broker directly; this surface only pulls
			// the externally-placed state back into TMS under the EXTERNAL book and
			// reconciles it vs the strategy books.
			//
			// DIRECTION 2 (broker -> TMS): pull the account's ACTUAL state + reflect
			// it into TMS + reconcile. READ-ONLY at the broker (places NO orders),
			// audited, safe in ALL modes incl signal. 503 when no broker-connected
			// session is present.
			r.Post("/trade/sync", s.handleTradeSync)
			// Sync status: a dedicated availability probe (the e2e skip-guard reads
			// this, NOT the always-present account reader). 503 when no session.
			r.Get("/trade/status", s.handleTradeStatus)
			// Account view reuses the trade account read surface.
			r.Get("/trade/account", s.handleTradeAccount)
		})
	})
	return r
}

// redirectTo returns a handler that 308-redirects to target, preserving the
// query string AND the HTTP method. It backs the old /live/* → /trade/* back-
// compat aliases so a not-yet-updated client keeps working through the rename.
// 308 (not 301): a 301 makes clients downgrade a POST to GET on follow, which
// turned POST /live/commands into GET /trade/commands → 404 (the control surface
// is POST-only). 308 preserves the method so the alias works for mutations too.
func redirectTo(target string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		dst := target
		if r.URL.RawQuery != "" {
			dst += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, dst, http.StatusPermanentRedirect)
	}
}

// depStatus is one dependency's health in the /healthz body.
type depStatus struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// handleHealthz reports process liveness plus dependency reachability.
// The status code is 200 even when a dependency is down ("degraded"):
// restarting the API cannot heal Postgres/Redis, so the container
// healthcheck only asserts the process serves HTTP.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	probe := func(p PingFunc) depStatus {
		if p == nil {
			return depStatus{OK: false, Error: "not configured"}
		}
		ctx, cancel := context.WithTimeout(r.Context(), depPingTimeout)
		defer cancel()
		if err := p(ctx); err != nil {
			return depStatus{OK: false, Error: err.Error()}
		}
		return depStatus{OK: true}
	}

	var (
		wg       sync.WaitGroup
		pg, reds depStatus
	)
	wg.Add(2)
	go func() { defer wg.Done(); pg = probe(s.pingPG) }()
	go func() { defer wg.Done(); reds = probe(s.pingRedis) }()
	wg.Wait()

	status := "ok"
	if !pg.OK || !reds.OK {
		status = "degraded"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  status,
		"version": app.Version,
		"deps": map[string]depStatus{
			"postgres": pg,
			"redis":    reds,
		},
	})
}
