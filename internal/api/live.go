package api

// live.go adds the live cockpit read surface + the audited command-enqueue
// endpoint (P5 task 3 + 4). The trading mutation surface stays OUT of the HTTP
// API (api spec §1.1 — read-only forever); the ONLY write here is enqueuing an
// ops.commands row (POST /api/v1/live/commands), which the tms-live consumer
// executes under full audit. Reads come from PG (the durable truth, decision 5):
//
//	GET  /api/v1/live/session   — latest session state (mode, status, halt)
//	GET  /api/v1/live/intents   — recent signal intents (from tms.signal_intents)
//	GET  /api/v1/live/health    — latest PortfolioHealth snapshot
//	GET  /api/v1/watchlist      — the live universe (latest session's instruments)
//	POST /api/v1/live/commands  — enqueue a control command (audited)

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/commands"
)

// LiveReader is the live cockpit read surface (PG-backed; satisfied by
// *LiveStore). All reads are best-effort newest-wins snapshots.
type LiveReader interface {
	// LatestSession returns the most recent session (any status), or nil when
	// none exists.
	LatestSession(ctx context.Context) (*LiveSession, error)
	// RecentIntents returns up to limit newest signal intents, optionally
	// filtered by strategy_id ("" = any).
	RecentIntents(ctx context.Context, strategyID string, limit int) ([]LiveIntent, error)
	// LatestHealth returns the newest PortfolioHealth snapshot, or nil when none.
	LatestHealth(ctx context.Context) (*LiveHealth, error)
	// Watchlist returns the distinct symbols the latest session is tracking.
	Watchlist(ctx context.Context) ([]string, error)
}

// CommandEnqueuer enqueues an audited control command (satisfied by
// *commands.Enqueuer). It is the ONLY write path of the live API.
type CommandEnqueuer interface {
	Enqueue(ctx context.Context, p commands.EnqueueParams) (int64, error)
}

// LiveSession is the wire shape of a live session.
type LiveSession struct {
	ID        int64           `json:"id"`
	TraderID  string          `json:"trader_id"`
	Mode      string          `json:"mode"`
	Status    string          `json:"status"`
	StartedAt time.Time       `json:"started_at"`
	EndedAt   *time.Time      `json:"ended_at"`
	Config    json.RawMessage `json:"config"`
	// Halt is the active halt (nil when none active).
	Halt *LiveHalt `json:"halt"`
}

// LiveHalt is the active halt summary.
type LiveHalt struct {
	Kind        string    `json:"kind"`
	Reason      string    `json:"reason"`
	TriggeredAt time.Time `json:"triggered_at"`
}

// LiveIntent is the wire shape of one recent signal intent.
type LiveIntent struct {
	StrategyID string          `json:"strategy_id"`
	Symbol     string          `json:"symbol"`
	State      string          `json:"state"`
	Strength   float64         `json:"strength"`
	Generation int64           `json:"generation"`
	Intent     json.RawMessage `json:"intent"`
	TS         time.Time       `json:"ts"`
	TSEventNS  int64           `json:"ts_event"`
}

// LiveHealth is the wire shape of the latest portfolio-health snapshot.
type LiveHealth struct {
	DayPnL           float64   `json:"day_pnl"`
	DayPnLPct        float64   `json:"day_pnl_pct"`
	DailyLossHalt    bool      `json:"daily_loss_halt"`
	HaltHeadroomPct  float64   `json:"halt_headroom_pct"`
	ConcentrationPct float64   `json:"concentration_pct"`
	TS               time.Time `json:"ts"`
}

// handleLiveSession serves GET /api/v1/live/session.
func (s *Server) handleLiveSession(w http.ResponseWriter, r *http.Request) {
	if s.live == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "live reader not configured")
		return
	}
	sess, err := s.live.LatestSession(r.Context())
	if err != nil {
		internalError(w, s.log, "live session", err)
		return
	}
	if sess == nil {
		writeJSON(w, http.StatusOK, map[string]any{"session": nil})
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

// handleLiveIntents serves GET /api/v1/live/intents?strategy_id=&limit=.
func (s *Server) handleLiveIntents(w http.ResponseWriter, r *http.Request) {
	if s.live == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "live reader not configured")
		return
	}
	strategyID := strings.TrimSpace(r.URL.Query().Get("strategy_id"))
	limit, ok := parseLimit(w, r, 100, 1000)
	if !ok {
		return
	}
	intents, err := s.live.RecentIntents(r.Context(), strategyID, limit)
	if err != nil {
		internalError(w, s.log, "live intents", err)
		return
	}
	if intents == nil {
		intents = []LiveIntent{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"intents": intents})
}

// handleLiveHealth serves GET /api/v1/live/health.
func (s *Server) handleLiveHealth(w http.ResponseWriter, r *http.Request) {
	if s.live == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "live reader not configured")
		return
	}
	health, err := s.live.LatestHealth(r.Context())
	if err != nil {
		internalError(w, s.log, "live health", err)
		return
	}
	if health == nil {
		writeError(w, http.StatusServiceUnavailable, "no_health",
			"PortfolioHealth stream is empty — no live producer running")
		return
	}
	writeJSON(w, http.StatusOK, health)
}

// handleWatchlist serves GET /api/v1/watchlist.
func (s *Server) handleWatchlist(w http.ResponseWriter, r *http.Request) {
	if s.live == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "live reader not configured")
		return
	}
	syms, err := s.live.Watchlist(r.Context())
	if err != nil {
		internalError(w, s.log, "watchlist", err)
		return
	}
	if syms == nil {
		syms = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"symbols": syms})
}

// liveCommandBody is the POST /api/v1/live/commands request shape.
type liveCommandBody struct {
	Name         string `json:"name"`
	Mode         string `json:"mode,omitempty"`
	Reason       string `json:"reason,omitempty"`
	ConfirmToken string `json:"confirm_token,omitempty"`
}

// handleLiveCommand serves POST /api/v1/live/commands: enqueue an audited
// control command. kill/halt/resume/stop/start are always allowed; set_mode to
// paper/live requires a confirmation token (returns 412 without one).
func (s *Server) handleLiveCommand(w http.ResponseWriter, r *http.Request) {
	if s.commands == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "command enqueuer not configured")
		return
	}
	var body liveCommandBody
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "invalid command body: "+err.Error())
		return
	}
	name := commands.Name(strings.TrimSpace(body.Name))
	if !name.IsValid() {
		writeError(w, http.StatusBadRequest, "invalid_command",
			"unknown command (want start|stop|set_mode|halt|resume|kill)")
		return
	}
	actor := actorFromRequest(r)
	id, err := s.commands.Enqueue(r.Context(), commands.EnqueueParams{
		Source: "api",
		Name:   name,
		Args: commands.CommandArgs{
			Mode:         strings.TrimSpace(body.Mode),
			Reason:       body.Reason,
			ConfirmToken: body.ConfirmToken,
		},
		RequestedBy: actor,
	})
	if errors.Is(err, commands.ErrConfirmationRequired) {
		writeError(w, http.StatusPreconditionFailed, "confirmation_required",
			"paper/live mode switch requires a confirm_token")
		return
	}
	if err != nil {
		// Validation errors (bad mode) are client errors; everything else is 500.
		if strings.Contains(err.Error(), "set_mode requires") || strings.Contains(err.Error(), "unknown command") {
			writeError(w, http.StatusBadRequest, "invalid_command", err.Error())
			return
		}
		internalError(w, s.log, "enqueue command", err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"command_id": id, "status": "pending"})
}

// actorFromRequest derives an audit actor from the request (the bearer-auth'd
// caller has no user id; we record the remote + a fixed "api-user" tag — the
// audit row's source already says "api").
func actorFromRequest(r *http.Request) string {
	remote := r.RemoteAddr
	if remote == "" {
		remote = "unknown"
	}
	return "api-user@" + remote
}
