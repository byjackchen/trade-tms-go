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
	Calendar    *calendar.Calendar
	PingPG      PingFunc
	PingRedis   PingFunc
	// Live is the live cockpit read surface (PG-backed). Optional: when nil the
	// /api/v1/live/* read endpoints return 503.
	Live LiveReader
	// Commands enqueues audited control commands. Optional: when nil
	// POST /api/v1/live/commands returns 503.
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
	// Preflight runs the go-live preflight for GET /api/v1/live/preflight.
	// Optional: when nil that endpoint returns 503 (preflight not wired).
	Preflight PreflightRunner
	// Manual is the operator-driven manual trade desk (the ONLY broker-mutation
	// surface) when the API process ITSELF holds a desk (an in-process deployment;
	// today only the unit tests). Optional: when nil AND no ManualProxy is wired the
	// /api/v1/trade/* endpoints return 503 (no manual session connected). SAFETY: a
	// non-nil desk has ALREADY satisfied the 4-factor live activation for a live
	// account; the per-order confirm + risk gate run inside the desk.
	Manual ManualTrader
	// ManualProxy reverse-proxies /api/v1/trade/* onto the live node's manual
	// listener (the broker-connected process). This is the SHIPPED compose topology:
	// the desk runs in the live node and the API fronts it on one host. Optional:
	// when nil the API falls back to an in-process Manual desk, else 503. When BOTH
	// are set the proxy wins (the live node is the real broker-connected surface).
	ManualProxy *ManualTradeProxy
	// Now overrides the clock (tests); nil = time.Now.
	Now func() time.Time
}

// Server is the HTTP/WebSocket API for the UI (contract: docs/api.md).
type Server struct {
	log         zerolog.Logger
	token       string
	corsOrigins []string
	jobs        JobQueue
	data        DataStore
	uni         UniverseReader
	runs        RunsReader
	strat       StrategyReader
	hyperopt    HyperoptReader
	promoter    HyperoptPromoter
	cal         *calendar.Calendar
	pingPG      PingFunc
	pingRedis   PingFunc
	live        LiveReader
	commands    CommandEnqueuer
	system      SystemReader
	audit       AuditReader
	sync        SyncForcer
	preflight   PreflightRunner
	manual      ManualTrader
	manualProxy *ManualTradeProxy
	hub         *Hub
	now         func() time.Time
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
		log:         log,
		token:       d.Token,
		corsOrigins: d.CORSOrigins,
		jobs:        d.Jobs,
		data:        d.Data,
		uni:         d.Universe,
		runs:        d.Runs,
		strat:       d.Strategies,
		hyperopt:    d.Hyperopt,
		promoter:    d.Promoter,
		cal:         d.Calendar,
		pingPG:      d.PingPG,
		pingRedis:   d.PingRedis,
		live:        d.Live,
		commands:    d.Commands,
		system:      d.System,
		audit:       d.Audit,
		sync:        d.Sync,
		preflight:   d.Preflight,
		manual:      d.Manual,
		manualProxy: d.ManualProxy,
		hub:         NewHub(log, d.CORSOrigins),
		now:         now,
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

			// Live cockpit (P5): read surface from PG + the audited command
			// enqueue endpoint. The trading mutation surface stays out of the
			// HTTP API (read-only forever); commands are the audited side channel.
			r.Get("/live/session", s.handleLiveSession)
			r.Get("/live/intents", s.handleLiveIntents)
			r.Get("/live/health", s.handleLiveHealth)
			r.Get("/live/preflight", s.handleLivePreflight)
			r.Get("/watchlist", s.handleWatchlist)
			// Paper/live trading read surface (P6 task 6).
			r.Get("/live/orders", s.handleLiveOrders)
			r.Get("/live/fills", s.handleLiveFills)
			r.Get("/live/positions", s.handleLivePositions)
			r.Get("/live/account", s.handleLiveAccount)
			r.Get("/live/reconciliation", s.handleLiveReconciliation)
			r.Post("/live/commands", s.handleLiveCommand)

			// MANUAL trade-mutation surface (operator-driven discretionary desk):
			// the ONLY broker-write path in the API. Each endpoint is gated inside
			// the desk (4-factor live activation + per-order confirm + risk gate +
			// audit). 412 confirmation_required / 422 risk_violation / 503 when no
			// manual desk is connected.
			//
			// TOPOLOGY: the desk lives in the broker-connected LIVE NODE, not this
			// API process, so when a ManualProxy upstream is wired (the shipped
			// compose stack) EVERY /trade/* route — incl GET account/status — is
			// reverse-proxied onto the live node's manual listener, giving the UI +
			// e2e suite one host. When no proxy is wired the API falls back to an
			// in-process desk (today only the unit tests) via the chi handlers.
			if s.manualProxy != nil {
				r.Handle("/trade/*", http.HandlerFunc(s.handleTradeProxy))
			} else {
				r.Post("/trade/order", s.handleTradeOrder)
				r.Post("/trade/order/{coid}/cancel", s.handleTradeCancel)
				r.Post("/trade/position/{symbol}/close", s.handleTradeClose)
				// DIRECTION 2 (broker -> TMS): pull the account's ACTUAL state +
				// reflect it into TMS + reconcile. READ-ONLY at the broker (places NO
				// orders), audited, safe in ALL modes incl signal.
				r.Post("/trade/sync", s.handleTradeSync)
				// Desk status: a dedicated availability probe on the actual mutation
				// surface (the e2e skip-guard reads this, NOT the always-present
				// account reader). 503 when no desk is connected.
				r.Get("/trade/status", s.handleTradeStatus)
				// Account view reuses the live account read surface.
				r.Get("/trade/account", s.handleLiveAccount)
			}
		})
	})
	return r
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
