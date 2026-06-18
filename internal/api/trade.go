package api

// trade.go adds the trade cockpit read surface + the audited command-enqueue
// endpoint (P5 task 3 + 4). The trading mutation surface stays OUT of the HTTP
// API (api spec §1.1 — read-only forever); the ONLY write here is enqueuing an
// ops.commands row (POST /api/v1/trade/commands), which the trade-node consumer
// executes under full audit. Reads come from PG (the durable truth, decision 5):
//
//	GET  /api/v1/trade/session   — latest session state (mode, status, halt)
//	GET  /api/v1/trade/signals   — recent signals (from tms.signals)
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
	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// TradeReader is the trade cockpit read surface (PG-backed; satisfied by
// *apistore.TradeStore). All reads are best-effort newest-wins snapshots.
type TradeReader interface {
	// LatestSession returns the most recent session (any status), or nil when
	// none exists.
	LatestSession(ctx context.Context) (*TradeSession, error)
	// RecentSignals returns up to limit newest signals, optionally
	// filtered by strategy_id ("" = any).
	RecentSignals(ctx context.Context, strategyID string, limit int) ([]TradeSignal, error)
	// LatestHealth returns the newest PortfolioHealth snapshot, or nil when none.
	LatestHealth(ctx context.Context) (*TradeHealth, error)
	// Watchlist returns the distinct symbols the latest session is tracking.
	Watchlist(ctx context.Context) ([]string, error)
	// LatestSignalsBySymbol returns the latest signal per symbol in the data
	// frontier window, ranked actionable-first (see apistore). It backs the
	// watchlist's per-symbol state so the whole universe shows its signal in one
	// read and actionable names rank to the top.
	LatestSignalsBySymbol(ctx context.Context, limit int) ([]TradeSignal, error)
	// ListAccounts returns the registered trading accounts (the tms.accounts
	// registry) for the UI account selector / per-account filter.
	ListAccounts(ctx context.Context) ([]TradeAccountInfo, error)
}

// TradeAccountInfo is the wire shape of one registered trading account (the
// tms.accounts registry row). It is DISTINCT from TradeAccount, which is the
// account's funds/buying-power snapshot.
type TradeAccountInfo struct {
	ID          string `json:"id"`
	Venue       string `json:"venue"`
	Env         string `json:"env"`
	BrokerAccID int64  `json:"broker_acc_id"`
	Label       string `json:"label"`
	// Kind is the derived operator word ("paper"|"live"), computed from Env via
	// domain.AccountKind (env=real => "live", else "paper"). It is NOT stored — the
	// env stays the source of truth; the unified /trade UI badges accounts from it.
	Kind string `json:"kind"`
}

// CommandEnqueuer enqueues an audited control command (satisfied by
// *commands.Enqueuer). It is the ONLY write path of the trade API.
type CommandEnqueuer interface {
	Enqueue(ctx context.Context, p commands.EnqueueParams) (int64, error)
}

// TradeSession is the wire shape of a trade session. The legacy three-valued
// "mode" is replaced by the 2D model (docs/concept-alignment.md §1.3): ExecPolicy
// (signal/auto) on the execution axis + AccountEnv (sim/simulate/real, from the
// bound account) on the environment axis.
type TradeSession struct {
	ID       int64  `json:"id"`
	TraderID string `json:"trader_id"`
	// ExecPolicy is the execution policy: "signal" (emit-only) | "auto" (auto-submit).
	ExecPolicy string `json:"exec_policy"`
	// AccountEnv is the bound account's env: "sim" | "simulate" | "real" (empty
	// when the session has no bound account).
	AccountEnv string `json:"account_env"`
	// CompositionID is the Composition this session runs (its strategies + weights
	// + risk). CompositionName is the human label for it. Empty when the session
	// carries no composition.
	CompositionID   string `json:"composition_id"`
	CompositionName string `json:"composition_name"`
	// AccountID is the bound broker account id ("<venue>:<env>:<acc>"). Empty in
	// signal mode (no account bound). The session is the join that ties a
	// Composition to the Account it executes on.
	AccountID  string          `json:"account_id"`
	Status     string          `json:"status"`
	StartedAt  time.Time       `json:"started_at"`
	EndedAt    *time.Time      `json:"ended_at"`
	Config     json.RawMessage `json:"config"`
	// Halt is the active halt (nil when none active).
	Halt *TradeHalt `json:"halt"`
}

// TradeHalt is the active halt summary.
type TradeHalt struct {
	Kind        string    `json:"kind"`
	Reason      string    `json:"reason"`
	TriggeredAt time.Time `json:"triggered_at"`
}

// TradeSignal is the wire shape of one recent signal.
type TradeSignal struct {
	StrategyID string          `json:"strategy_id"`
	Symbol     string          `json:"symbol"`
	State      string          `json:"state"`
	Strength   float64         `json:"strength"`
	Generation int64           `json:"generation"`
	Signal     json.RawMessage `json:"signal"`
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

// handleTradeSignals serves GET /api/v1/trade/signals?strategy_id=&limit=.
func (s *Server) handleTradeSignals(w http.ResponseWriter, r *http.Request) {
	if s.trade == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "trade reader not configured")
		return
	}
	strategyID := strings.TrimSpace(r.URL.Query().Get("strategy_id"))
	limit, ok := parseLimit(w, r, 100, 1000)
	if !ok {
		return
	}
	signals, err := s.trade.RecentSignals(r.Context(), strategyID, limit)
	if err != nil {
		internalError(w, s.log, "trade signals", err)
		return
	}
	if signals == nil {
		signals = []TradeSignal{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"signals": signals})
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
	// Join the latest signal per symbol (frontier-windowed, actionable-first) so
	// every rendered row carries its current signal state without a separate
	// capped signals poll. Additive: `symbols` stays for back-compat (WS frame +
	// the e2e suite); `signals` enriches the rows the UI shows.
	signals, err := s.trade.LatestSignalsBySymbol(r.Context(), 5000)
	if err != nil {
		internalError(w, s.log, "watchlist signals", err)
		return
	}
	if signals == nil {
		signals = []TradeSignal{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"symbols": syms, "signals": signals})
}

// handleTradeAccounts serves GET /api/v1/trade/accounts: the registered trading
// accounts (tms.accounts registry) for the UI account selector / per-account
// filter.
func (s *Server) handleTradeAccounts(w http.ResponseWriter, r *http.Request) {
	if s.trade == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "trade reader not configured")
		return
	}
	accounts, err := s.trade.ListAccounts(r.Context())
	if err != nil {
		internalError(w, s.log, "trade accounts", err)
		return
	}
	if accounts == nil {
		accounts = []TradeAccountInfo{}
	}
	// Derive each account's kind ("paper"|"live") from its env (the UI badge reads
	// it; env stays the source of truth). The store leaves Kind empty.
	for i := range accounts {
		accounts[i].Kind = domain.AccountKind(domain.BrokerEnv(accounts[i].Env))
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": accounts})
}

// tradeCommandBody is the POST /api/v1/trade/commands request shape. The legacy
// three-valued mode is replaced by the 2D model (docs §1.3): set_mode now takes
// exec_policy (signal|auto) + an account selector (env: sim|simulate|real). "go
// paper" = exec_policy=auto + env=simulate; "go live" = auto + env=real; "signal"
// = exec_policy=signal (env ignored).
type tradeCommandBody struct {
	Name string `json:"name"`
	// ExecPolicy is the execution policy for set_mode ("signal" | "auto").
	ExecPolicy string `json:"exec_policy,omitempty"`
	// Env is the bound account env for set_mode with exec_policy=auto
	// ("sim" | "simulate" | "real"). Ignored for exec_policy=signal.
	Env          string `json:"env,omitempty"`
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
	// set_mode's control input is (exec_policy, env); derive the convenience word
	// the command queue still carries as its restart vocabulary.
	mode := ""
	if name == commands.NameSetMode {
		m, ok := deriveSetModeWord(w, body.ExecPolicy, body.Env)
		if !ok {
			return
		}
		mode = m
	}
	actor := actorFromRequest(r)
	id, err := s.commands.Enqueue(r.Context(), commands.EnqueueParams{
		Source: "api",
		Name:   name,
		Args: commands.CommandArgs{
			Mode:         mode,
			Reason:       body.Reason,
			ConfirmToken: body.ConfirmToken,
		},
		RequestedBy: actor,
	})
	if errors.Is(err, commands.ErrConfirmationRequired) {
		writeError(w, http.StatusPreconditionFailed, "confirmation_required",
			"this command requires a confirm_token (auto/live exec switch, flatten, or emergency_kill)")
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

// deriveSetModeWord validates a set_mode control input (exec_policy + env) and
// derives the convenience word (signal|paper|live) the command queue carries.
// exec_policy=signal needs no account (env ignored); exec_policy=auto requires an
// env (simulate => "paper", real => "live"). Returns ok=false after writing an
// error response.
func deriveSetModeWord(w http.ResponseWriter, execPolicy, env string) (string, bool) {
	exec, err := domain.ParseExecutionPolicy(strings.TrimSpace(execPolicy))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_exec_policy", "exec_policy must be signal|auto")
		return "", false
	}
	if exec == domain.ExecSignal {
		return domain.RunWord(exec, ""), true
	}
	e := domain.BrokerEnv(strings.TrimSpace(env))
	if e == "" {
		writeError(w, http.StatusBadRequest, "missing_account",
			"set_mode with exec_policy=auto requires env (simulate for paper, real for live)")
		return "", false
	}
	if !e.IsValid() {
		writeError(w, http.StatusBadRequest, "bad_env", "env must be sim|simulate|real")
		return "", false
	}
	return domain.RunWord(exec, e), true
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
