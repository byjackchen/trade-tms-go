package api

// trade.go adds the trade cockpit read surface + the audited command-enqueue
// endpoint (P5 task 3 + 4). The trading mutation surface stays OUT of the HTTP
// API (api spec §1.1 — read-only forever); the ONLY write here is enqueuing an
// ops.commands row (POST /api/v1/trade/commands), which the trade-node consumer
// executes under full audit. Reads come from PG (the durable truth, decision 5):
//
//	GET  /api/v1/trade/session   — latest session state (mode, status, halt)
//	GET  /api/v1/trade/intents   — recent signal intents (from tms.signal_intents)
//	GET  /api/v1/trade/health    — latest PortfolioHealth snapshot
//	GET  /api/v1/watchlist       — the trade universe (latest session's instruments)
//	POST /api/v1/trade/commands  — enqueue a control command (audited)

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/commands"
)

// TradeReader is the trade cockpit read surface (PG-backed; satisfied by
// *apistore.TradeStore). All reads are best-effort newest-wins snapshots.
type TradeReader interface {
	// LatestSession returns the most recent session (any status), or nil when
	// none exists.
	LatestSession(ctx context.Context) (*TradeSession, error)
	// RecentIntents returns up to limit newest signal intents, optionally
	// filtered by strategy_id ("" = any).
	RecentIntents(ctx context.Context, strategyID string, limit int) ([]TradeIntent, error)
	// LatestHealth returns the newest PortfolioHealth snapshot, or nil when none.
	LatestHealth(ctx context.Context) (*TradeHealth, error)
	// Watchlist returns the distinct symbols the latest session is tracking.
	Watchlist(ctx context.Context) ([]string, error)
	// LatestIntentsBySymbol returns the latest intent per symbol in the data
	// frontier window, ranked actionable-first (see apistore). It backs the
	// watchlist's per-symbol state so the whole universe shows its signal in one
	// read and actionable names rank to the top.
	LatestIntentsBySymbol(ctx context.Context, limit int) ([]TradeIntent, error)
}

// CommandEnqueuer enqueues an audited control command (satisfied by
// *commands.Enqueuer). It is the ONLY write path of the trade API.
type CommandEnqueuer interface {
	Enqueue(ctx context.Context, p commands.EnqueueParams) (int64, error)
}

// TradeSession is the wire shape of a trade session.
type TradeSession struct {
	ID        int64           `json:"id"`
	TraderID  string          `json:"trader_id"`
	Mode      string          `json:"mode"`
	Status    string          `json:"status"`
	StartedAt time.Time       `json:"started_at"`
	EndedAt   *time.Time      `json:"ended_at"`
	Config    json.RawMessage `json:"config"`
	// Halt is the active halt (nil when none active).
	Halt *TradeHalt `json:"halt"`
}

// TradeHalt is the active halt summary.
type TradeHalt struct {
	Kind        string    `json:"kind"`
	Reason      string    `json:"reason"`
	TriggeredAt time.Time `json:"triggered_at"`
}

// TradeIntent is the wire shape of one recent signal intent.
type TradeIntent struct {
	StrategyID string          `json:"strategy_id"`
	Symbol     string          `json:"symbol"`
	State      string          `json:"state"`
	Strength   float64         `json:"strength"`
	Generation int64           `json:"generation"`
	Intent     json.RawMessage `json:"intent"`
	TS         time.Time       `json:"ts"`
	TSEventNS  int64           `json:"ts_event"`
}

// TradeHealth is the wire shape of the latest portfolio-health snapshot.
type TradeHealth struct {
	DayPnL           float64   `json:"day_pnl"`
	DayPnLPct        float64   `json:"day_pnl_pct"`
	DailyLossHalt    bool      `json:"daily_loss_halt"`
	HaltHeadroomPct  float64   `json:"halt_headroom_pct"`
	ConcentrationPct float64   `json:"concentration_pct"`
	TS               time.Time `json:"ts"`
}

// handleTradeSession serves GET /api/v1/trade/session.
func (s *Server) handleTradeSession(w http.ResponseWriter, r *http.Request) {
	if s.trade == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "trade reader not configured")
		return
	}
	sess, err := s.trade.LatestSession(r.Context())
	if err != nil {
		internalError(w, s.log, "trade session", err)
		return
	}
	if sess == nil {
		writeJSON(w, http.StatusOK, map[string]any{"session": nil})
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

// handleTradeIntents serves GET /api/v1/trade/intents?strategy_id=&limit=.
func (s *Server) handleTradeIntents(w http.ResponseWriter, r *http.Request) {
	if s.trade == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "trade reader not configured")
		return
	}
	strategyID := strings.TrimSpace(r.URL.Query().Get("strategy_id"))
	limit, ok := parseLimit(w, r, 100, 1000)
	if !ok {
		return
	}
	intents, err := s.trade.RecentIntents(r.Context(), strategyID, limit)
	if err != nil {
		internalError(w, s.log, "trade intents", err)
		return
	}
	if intents == nil {
		intents = []TradeIntent{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"intents": intents})
}

// handleTradeHealth serves GET /api/v1/trade/health.
func (s *Server) handleTradeHealth(w http.ResponseWriter, r *http.Request) {
	if s.trade == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "trade reader not configured")
		return
	}
	health, err := s.trade.LatestHealth(r.Context())
	if err != nil {
		internalError(w, s.log, "trade health", err)
		return
	}
	if health == nil {
		writeError(w, http.StatusServiceUnavailable, "no_health",
			"PortfolioHealth stream is empty — no trade producer running")
		return
	}
	writeJSON(w, http.StatusOK, health)
}

// handleWatchlist serves GET /api/v1/watchlist.
func (s *Server) handleWatchlist(w http.ResponseWriter, r *http.Request) {
	if s.trade == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "trade reader not configured")
		return
	}
	syms, err := s.trade.Watchlist(r.Context())
	if err != nil {
		internalError(w, s.log, "watchlist", err)
		return
	}
	if syms == nil {
		syms = []string{}
	}
	// Join the latest intent per symbol (frontier-windowed, actionable-first) so
	// every rendered row carries its current signal state without a separate
	// capped intents poll. Additive: `symbols` stays for back-compat (WS frame +
	// the e2e suite); `intents` enriches the rows the UI shows.
	intents, err := s.trade.LatestIntentsBySymbol(r.Context(), 5000)
	if err != nil {
		internalError(w, s.log, "watchlist intents", err)
		return
	}
	if intents == nil {
		intents = []TradeIntent{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"symbols": syms, "intents": intents})
}

// tradeCommandBody is the POST /api/v1/trade/commands request shape.
type tradeCommandBody struct {
	Name         string `json:"name"`
	Mode         string `json:"mode,omitempty"`
	Reason       string `json:"reason,omitempty"`
	ConfirmToken string `json:"confirm_token,omitempty"`
}

// handleTradeCommand serves POST /api/v1/trade/commands: enqueue an audited
// control command. kill/halt/resume/stop/start are always allowed; set_mode to
// paper/live requires a confirmation token (returns 412 without one).
func (s *Server) handleTradeCommand(w http.ResponseWriter, r *http.Request) {
	if s.commands == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "command enqueuer not configured")
		return
	}
	var body tradeCommandBody
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "invalid command body: "+err.Error())
		return
	}
	name := commands.Name(strings.TrimSpace(body.Name))
	if !name.IsValid() {
		writeError(w, http.StatusBadRequest, "invalid_command",
			"unknown command (want start|stop|set_mode|halt|resume|kill|flatten|emergency_kill|reconcile)")
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
			"this command requires a confirm_token (paper/live mode switch, flatten, or emergency_kill)")
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
