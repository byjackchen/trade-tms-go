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

			r.Get("/jobs", s.handleJobList)
			r.Get("/jobs/{id}", s.handleJobGet)
			r.Post("/jobs/{id}/cancel", s.handleJobCancel)

			r.Get("/universe/latest", s.handleUniverseLatest)
			r.Post("/universe/rebuild", s.handleUniverseRebuild)

			r.Get("/strategies", s.handleStrategyList)
			r.Get("/strategies/{id}", s.handleStrategyGet)

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
