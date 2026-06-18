package api

// preflight.go serves the go-live PREFLIGHT report over HTTP so the UI System
// page can render the precondition checks (data freshness, per-strategy warmup,
// market caps, universe, OpenD, PG/Redis) without an operator shelling into the
// node. Read-only + bearer-guarded like the rest of /api/v1.
//
//	GET /api/v1/trade/preflight?exec_policy=&env=&strategy=&tickers=&orb_symbol=&check_opend=&max_stale_days=
//
// The Server holds a PreflightRunner (wired in cmd to the real PG/sharadar/moomoo
// probes); when nil the endpoint returns 503 (preflight not configured for this
// deployment).

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// PreflightResult is the wire shape of one precondition check (mirrors
// preflight.CheckResult; redeclared here so the api package does not leak the
// preflight types into its public contract).
type PreflightResult struct {
	Check    string `json:"check"`
	Status   string `json:"status"`
	Severity string `json:"severity"`
	Detail   string `json:"detail"`
}

// PreflightReport is the wire shape of the preflight outcome.
type PreflightReport struct {
	// ExecPolicy is the execution policy validated (signal|auto).
	ExecPolicy string `json:"exec_policy"`
	// Env is the bound account env validated (paper|real; empty when none).
	Env string `json:"env"`
	// RunWord is the derived convenience label (signal|paper|live), always
	// derived from (exec_policy, env) — display-only, never an input.
	RunWord  string            `json:"run_word"`
	Strategy string            `json:"strategy"`
	TS       string            `json:"ts"`
	OK       bool              `json:"ok"`
	Checks   []PreflightResult `json:"checks"`
}

// PreflightParams selects the session the preflight validates. The legacy
// three-valued mode is replaced by the 2D model (docs §1.3): ExecPolicy on the
// execution axis + Env (the bound account's env) on the environment axis.
type PreflightParams struct {
	// ExecPolicy is "signal" | "auto".
	ExecPolicy string
	// Env is the bound account env ("paper" | "real"; "" when none).
	Env                 string
	Strategy            string
	Tickers             []string
	ORBSymbol           string
	MaxStaleTradingDays int
	CheckOpenD          bool
}

// PreflightRunner runs the go-live preflight and returns the report. The api
// package depends only on this interface (the concrete probes live in
// internal/preflight, wired in cmd) so api stays free of the heavy data deps.
type PreflightRunner interface {
	RunPreflight(ctx context.Context, p PreflightParams) PreflightReport
}

// handleTradePreflight serves GET /api/v1/trade/preflight. HTTP is always 200 with
// the report in the body (the OK bool is the go/no-go bit); a failing preflight
// is a valid, expected response the UI renders, not an HTTP error.
func (s *Server) handleTradePreflight(w http.ResponseWriter, r *http.Request) {
	if s.preflight == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "preflight runner not configured")
		return
	}
	q := r.URL.Query()
	execPolicy := strings.TrimSpace(q.Get("exec_policy"))
	if execPolicy == "" {
		execPolicy = "signal"
	}
	switch execPolicy {
	case "signal", "auto":
	default:
		writeError(w, http.StatusBadRequest, "bad_exec_policy", "exec_policy must be signal|auto")
		return
	}
	// Env selects WHERE auto orders settle (a "go paper" = auto on a paper
	// account; "go live" = auto on a real account). Either pass env= directly or
	// account_id= to resolve it from the accounts registry. Signal has no account.
	env, ok := s.resolvePreflightEnv(w, r, q, execPolicy)
	if !ok {
		return
	}
	strategy := strings.TrimSpace(q.Get("strategy"))
	if strategy == "" {
		strategy = "multi"
	}
	switch strategy {
	case "sepa", "sector_rotation", "pairs", "orb", "multi":
	default:
		writeError(w, http.StatusBadRequest, "bad_strategy", "strategy must be sepa|sector_rotation|pairs|orb|multi")
		return
	}

	var tickers []string
	for _, t := range strings.Split(q.Get("tickers"), ",") {
		if t = strings.ToUpper(strings.TrimSpace(t)); t != "" {
			tickers = append(tickers, t)
		}
	}

	maxStale := 1
	if v := strings.TrimSpace(q.Get("max_stale_days")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "bad_max_stale", "max_stale_days must be a non-negative integer")
			return
		}
		maxStale = n
	}
	checkOpenD := parseBoolQuery(q.Get("check_opend"))

	report := s.preflight.RunPreflight(r.Context(), PreflightParams{
		ExecPolicy:          execPolicy,
		Env:                 env,
		Strategy:            strategy,
		Tickers:             tickers,
		ORBSymbol:           strings.TrimSpace(q.Get("orb_symbol")),
		MaxStaleTradingDays: maxStale,
		CheckOpenD:          checkOpenD,
	})
	writeJSON(w, http.StatusOK, report)
}

// resolvePreflightEnv resolves the bound-account env for a preflight request.
// signal runs settle nowhere (env is irrelevant; "" is returned). For auto, the
// caller selects the account either by env= directly or by account_id= (resolved
// from the accounts registry). Returns ok=false after writing an error response.
func (s *Server) resolvePreflightEnv(w http.ResponseWriter, r *http.Request, q url.Values, execPolicy string) (string, bool) {
	if execPolicy == "signal" {
		return "", true
	}
	env := strings.TrimSpace(q.Get("env"))
	if accID := strings.TrimSpace(q.Get("account_id")); accID != "" {
		if s.trade == nil {
			writeError(w, http.StatusServiceUnavailable, "unavailable", "account registry not configured")
			return "", false
		}
		accounts, err := s.trade.ListAccounts(r.Context())
		if err != nil {
			internalError(w, s.log, "preflight accounts", err)
			return "", false
		}
		found := ""
		for _, a := range accounts {
			if a.ID == accID {
				found = a.Env
				break
			}
		}
		if found == "" {
			writeError(w, http.StatusBadRequest, "unknown_account", "account_id not found in the accounts registry: "+accID)
			return "", false
		}
		if env != "" && env != found {
			writeError(w, http.StatusBadRequest, "env_conflict", "env does not match the selected account's env")
			return "", false
		}
		env = found
	}
	switch env {
	case "paper", "real":
		return env, true
	case "":
		writeError(w, http.StatusBadRequest, "missing_account",
			"exec_policy=auto requires an account selector (account_id or env=paper|real)")
		return "", false
	default:
		writeError(w, http.StatusBadRequest, "bad_env", "env must be paper|real")
		return "", false
	}
}

// parseBoolQuery treats "1"/"true"/"yes" (case-insensitive) as true.
func parseBoolQuery(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "y":
		return true
	default:
		return false
	}
}
