package api

// preflight.go serves the go-live PREFLIGHT report over HTTP so the UI System
// page can render the precondition checks (data freshness, per-strategy warmup,
// market caps, universe, OpenD, PG/Redis) without an operator shelling into the
// node. Read-only + bearer-guarded like the rest of /api/v1.
//
//	GET /api/v1/trade/preflight?mode=&strategy=&tickers=&orb_symbol=&check_opend=&max_stale_days=
//
// The Server holds a PreflightRunner (wired in cmd to the real PG/sharadar/moomoo
// probes); when nil the endpoint returns 503 (preflight not configured for this
// deployment).

import (
	"context"
	"net/http"
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
	Mode     string            `json:"mode"`
	Strategy string            `json:"strategy"`
	TS       string            `json:"ts"`
	OK       bool              `json:"ok"`
	Checks   []PreflightResult `json:"checks"`
}

// PreflightParams selects the session the preflight validates.
type PreflightParams struct {
	Mode                string
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
	mode := strings.TrimSpace(q.Get("mode"))
	if mode == "" {
		mode = "signal"
	}
	switch mode {
	case "signal", "paper", "live":
	default:
		writeError(w, http.StatusBadRequest, "bad_mode", "mode must be signal|paper|live")
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
		Mode:                mode,
		Strategy:            strategy,
		Tickers:             tickers,
		ORBSymbol:           strings.TrimSpace(q.Get("orb_symbol")),
		MaxStaleTradingDays: maxStale,
		CheckOpenD:          checkOpenD,
	})
	writeJSON(w, http.StatusOK, report)
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
