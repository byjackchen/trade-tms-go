package api

// handlers_backtests.go serves the backtest result + control plane:
//
//	POST /api/v1/backtests              enqueue a backtest.run job (202 + id, audited)
//	GET  /api/v1/backtests              list runs (newest-first)
//	GET  /api/v1/backtests/{id}         run detail: meta + portfolio/strategy metrics
//	GET  /api/v1/backtests/{id}/equity  equity-curve points (optional ?strategy=)
//	GET  /api/v1/backtests/{id}/trades  round-trip trades
//	GET  /api/v1/backtests/{id}/orders  submitted orders (opaque array)
//
// The DB (research.*) is the source of truth (P2 locked decision 4); these read
// it via runs.Store. Money is rendered as float64 USD (the legacy artifact
// shape the UI already consumes).

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/byjackchen/trade-tms-go/internal/composition"
	"github.com/byjackchen/trade-tms-go/internal/domain"
	"github.com/byjackchen/trade-tms-go/internal/jobs"
	"github.com/byjackchen/trade-tms-go/internal/jobs/handlers"
	"github.com/byjackchen/trade-tms-go/internal/metrics"
	"github.com/byjackchen/trade-tms-go/internal/runs"
)

const (
	defaultBacktestsLimit = 50
	maxBacktestsLimit     = 500
)

// ---------------------------------------------------------------------------
// POST /api/v1/backtests
// ---------------------------------------------------------------------------

// backtestRequest is the enqueue body. It is the backtest.run payload plus the
// queue-level actor / max_attempts / dedupe_key. Unknown fields are rejected.
//
// The strategy selector is GONE (docs/concept-alignment.md §3.3, A3): a backtest's
// object is always a Composition, so the request carries composition_id and the
// server resolves the blueprint, passing it through to the engine's
// assembleFromComposition. A single-strategy backtest is just a single-member
// Composition (e.g. "sepa-only"). The "scripted" intents path keeps working
// WITHOUT a composition_id (it bypasses strategy assembly entirely).
type backtestRequest struct {
	Tickers         []string         `json:"tickers"`
	Universe        map[string]any   `json:"universe"`
	Start           string           `json:"start"`
	End             string           `json:"end"`
	StartingBalance *float64         `json:"starting_balance"`
	FillProfile     string           `json:"fill_profile"`
	CompositionID   string           `json:"composition_id"`
	ORBSymbol       string           `json:"orb_symbol"`
	Intents         []map[string]any `json:"intents"`
	Kind            string           `json:"kind"`
	Seed            int64            `json:"seed"`
	RunTS           string           `json:"run_ts"`
	Realistic       map[string]any   `json:"realistic"`

	Actor       string `json:"actor"`
	MaxAttempts int32  `json:"max_attempts"`
	DedupeKey   string `json:"dedupe_key"`
}

func (s *Server) handleBacktestEnqueue(w http.ResponseWriter, r *http.Request) {
	var req backtestRequest
	if err := decodeStrictJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeValidation, err.Error())
		return
	}
	if strings.TrimSpace(req.Start) == "" || strings.TrimSpace(req.End) == "" {
		writeError(w, http.StatusBadRequest, CodeValidation, `"start" and "end" are required (YYYY-MM-DD)`)
		return
	}
	compositionID := strings.TrimSpace(req.CompositionID)
	// Two backtest shapes: a Composition-driven run (composition_id, the real
	// strategies) or a scripted run (intents, no composition). Exactly one selector
	// must be present.
	scripted := len(req.Intents) > 0 && compositionID == ""
	switch {
	case compositionID == "" && !scripted:
		writeError(w, http.StatusBadRequest, CodeValidation,
			`"composition_id" is required (single-strategy backtest = a single-member Composition id like "sepa-only")`)
		return
	case compositionID != "" && len(req.Intents) > 0:
		writeError(w, http.StatusBadRequest, CodeValidation, `"composition_id" and "intents" are mutually exclusive`)
		return
	}

	// Resolve the Composition up front (so a bad id is a clean 404 here, not a
	// deferred job failure) and pass the blueprint through to the engine's
	// assembleFromComposition. ORB-bearing Compositions need either orb_symbol or a
	// single ticker; SEPA/multi treat supplied tickers as the stock universe.
	var comp *composition.Composition
	if compositionID != "" {
		if s.compositions == nil {
			writeError(w, http.StatusServiceUnavailable, CodeInternal, "composition store not configured")
			return
		}
		resolved, err := s.compositions.Get(r.Context(), compositionID)
		if errors.Is(err, composition.ErrNotFound) {
			writeError(w, http.StatusNotFound, CodeNotFound, fmt.Sprintf("composition %q not found", compositionID))
			return
		}
		if err != nil {
			internalError(w, s.log, "backtest resolve composition", err)
			return
		}
		comp = resolved
		if backtestHasORB(comp) && strings.TrimSpace(req.ORBSymbol) == "" && len(req.Tickers) != 1 {
			writeError(w, http.StatusBadRequest, CodeValidation,
				`this Composition has an ORB member; supply "orb_symbol" (or exactly one ticker)`)
			return
		}
	} else if len(req.Tickers) == 0 && req.Universe == nil {
		// Scripted needs an explicit ticker list or universe window.
		writeError(w, http.StatusBadRequest, CodeValidation, `supply "tickers" or a "universe" window`)
		return
	}
	if req.FillProfile != "" && req.FillProfile != "nautilus-compat" && req.FillProfile != "realistic" {
		writeError(w, http.StatusBadRequest, CodeValidation,
			fmt.Sprintf("unknown fill_profile %q (want \"nautilus-compat\" or \"realistic\")", req.FillProfile))
		return
	}
	if req.MaxAttempts < 0 || req.MaxAttempts > 10 {
		writeError(w, http.StatusBadRequest, CodeValidation, "max_attempts must be in [0, 10] (0 = default 1)")
		return
	}

	// Build the job payload from the validated request (only present fields).
	payload := map[string]any{"start": req.Start, "end": req.End}
	if len(req.Tickers) > 0 {
		payload["tickers"] = req.Tickers
	}
	if req.Universe != nil {
		payload["universe"] = req.Universe
	}
	if req.StartingBalance != nil {
		payload["starting_balance"] = *req.StartingBalance
	}
	if req.FillProfile != "" {
		payload["fill_profile"] = req.FillProfile
	}
	if comp != nil {
		// The resolved blueprint travels in the payload so the worker assembles
		// from it directly (no second DB lookup, no legacy strategy= mapping).
		payload["composition_id"] = comp.ID
		payload["composition"] = comp
	}
	if s := strings.TrimSpace(req.ORBSymbol); s != "" {
		payload["orb_symbol"] = s
	}
	if req.Intents != nil {
		payload["intents"] = req.Intents
	}
	if req.Kind != "" {
		payload["kind"] = req.Kind
	}
	if req.Seed != 0 {
		payload["seed"] = req.Seed
	}
	if req.RunTS != "" {
		payload["run_ts"] = req.RunTS
	}
	if req.Realistic != nil {
		payload["realistic"] = req.Realistic
	}

	job, deduped, err := s.jobs.Enqueue(r.Context(), jobs.EnqueueParams{
		Kind:        handlers.KindBacktestRun,
		Payload:     payload,
		DedupeKey:   strings.TrimSpace(req.DedupeKey),
		MaxAttempts: req.MaxAttempts,
		Actor:       actorOrDefault(req.Actor),
	})
	if err != nil {
		internalError(w, s.log, "enqueue backtest.run", err)
		return
	}
	s.log.Info().Int64("job_id", job.ID).Bool("deduped", deduped).
		Str("actor", actorOrDefault(req.Actor)).Msg("backtest.run enqueued")
	writeJSON(w, http.StatusAccepted, map[string]any{"job": jobToJSON(job), "deduped": deduped})
}

// ---------------------------------------------------------------------------
// GET /api/v1/backtests
// ---------------------------------------------------------------------------

type runSummaryJSON struct {
	ID              int64    `json:"id"`
	RunTS           string   `json:"run_ts"`
	Kind            string   `json:"kind"`
	Status          string   `json:"status"`
	StartDate       string   `json:"start_date"`
	EndDate         string   `json:"end_date"`
	StartingBalance float64  `json:"starting_balance_usd"`
	FinalBalance    *float64 `json:"final_balance_usd"`
	TotalPnL        *float64 `json:"total_pnl_usd"`
	Strategies      []string `json:"strategies"`
	CreatedAt       string   `json:"created_at"`
	UpdatedAt       string   `json:"updated_at"`
}

func summaryToJSON(s runs.RunSummary) runSummaryJSON {
	strats := s.Strategies
	if strats == nil {
		strats = []string{}
	}
	return runSummaryJSON{
		ID:              s.ID,
		RunTS:           s.RunTS,
		Kind:            s.Kind,
		Status:          s.Status,
		StartDate:       s.StartDate,
		EndDate:         s.EndDate,
		StartingBalance: s.StartingBalance.Float64(),
		FinalBalance:    moneyFloatPtr(s.FinalBalance),
		TotalPnL:        moneyFloatPtr(s.TotalPnL),
		Strategies:      strats,
		CreatedAt:       s.CreatedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt:       s.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
}

func (s *Server) handleBacktestList(w http.ResponseWriter, r *http.Request) {
	limit, ok := parseLimit(w, r, defaultBacktestsLimit, maxBacktestsLimit)
	if !ok {
		return
	}
	kind := strings.TrimSpace(r.URL.Query().Get("kind"))
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	if status != "" && !validRunStatus(status) {
		writeError(w, http.StatusBadRequest, CodeValidation,
			fmt.Sprintf("unknown status %q (want RUNNING|COMPLETE|INTERRUPTED|FAIL)", status))
		return
	}
	list, err := s.runs.List(r.Context(), runs.ListFilter{Kind: kind, Status: status, Limit: limit})
	if err != nil {
		internalError(w, s.log, "backtest list", err)
		return
	}
	out := make([]runSummaryJSON, 0, len(list))
	for _, run := range list {
		out = append(out, summaryToJSON(run))
	}
	writeJSON(w, http.StatusOK, map[string]any{"backtests": out})
}

// ---------------------------------------------------------------------------
// GET /api/v1/backtests/{id}
// ---------------------------------------------------------------------------

type metricsJSON struct {
	FinalBalanceUSD   float64 `json:"final_balance_usd"`
	TotalPnLUSD       float64 `json:"total_pnl_usd"`
	Sharpe            float64 `json:"sharpe"`
	Calmar            float64 `json:"calmar"`
	MaxDrawdownPct    float64 `json:"max_drawdown_pct"`
	NumOrders         int     `json:"num_orders"`
	NumFilledOrders   int     `json:"num_filled_orders"`
	NumRejectedOrders int     `json:"num_rejected_orders"`
	NumPositions      int     `json:"num_positions"`
}

func metricsToJSON(m metrics.BacktestMetrics) metricsJSON {
	return metricsJSON{
		FinalBalanceUSD:   m.FinalBalanceUSD,
		TotalPnLUSD:       m.TotalPnLUSD,
		Sharpe:            m.Sharpe,
		Calmar:            m.Calmar,
		MaxDrawdownPct:    m.MaxDrawdownPct,
		NumOrders:         m.NumOrders,
		NumFilledOrders:   m.NumFilledOrders,
		NumRejectedOrders: m.NumRejectedOrders,
		NumPositions:      m.NumPositions,
	}
}

func (s *Server) handleBacktestGet(w http.ResponseWriter, r *http.Request) {
	id, ok := backtestIDParam(w, r)
	if !ok {
		return
	}
	d, err := s.runs.Get(r.Context(), id)
	if errors.Is(err, runs.ErrRunNotFound) {
		writeError(w, http.StatusNotFound, CodeNotFound, fmt.Sprintf("backtest %d not found", id))
		return
	}
	if err != nil {
		internalError(w, s.log, "backtest get", err)
		return
	}

	body := map[string]any{"backtest": summaryToJSON(d.RunSummary), "config": rawOrNull(d.Config)}
	if d.PortfolioMetrics != nil {
		body["metrics"] = metricsToJSON(*d.PortfolioMetrics)
	} else {
		body["metrics"] = nil
	}
	stratMetrics := make(map[string]metricsJSON, len(d.StrategyMetrics))
	for sid, m := range d.StrategyMetrics {
		stratMetrics[sid] = metricsToJSON(m)
	}
	body["strategy_metrics"] = stratMetrics
	writeJSON(w, http.StatusOK, body)
}

// ---------------------------------------------------------------------------
// GET /api/v1/backtests/{id}/equity?strategy=
// ---------------------------------------------------------------------------

type equityPointJSON struct {
	TS         string  `json:"ts"`
	BalanceUSD float64 `json:"balance_usd"`
}

func (s *Server) handleBacktestEquity(w http.ResponseWriter, r *http.Request) {
	id, ok := backtestIDParam(w, r)
	if !ok {
		return
	}
	scope := strings.TrimSpace(r.URL.Query().Get("strategy"))
	pts, err := s.runs.Equity(r.Context(), id, scope)
	if errors.Is(err, runs.ErrRunNotFound) {
		writeError(w, http.StatusNotFound, CodeNotFound, fmt.Sprintf("backtest %d not found", id))
		return
	}
	if err != nil {
		internalError(w, s.log, "backtest equity", err)
		return
	}
	out := make([]equityPointJSON, 0, len(pts))
	for _, p := range pts {
		out = append(out, equityPointJSON{
			TS:         p.TS.UTC().Format(time.RFC3339Nano),
			BalanceUSD: p.BalanceUSD.Float64(),
		})
	}
	resolved := scope
	if resolved == "" {
		resolved = "portfolio"
	}
	writeJSON(w, http.StatusOK, map[string]any{"scope": resolved, "points": out})
}

// ---------------------------------------------------------------------------
// GET /api/v1/backtests/{id}/trades
// ---------------------------------------------------------------------------

type tradeJSON struct {
	ID          int64    `json:"id"`
	StrategyID  string   `json:"strategy_id"`
	Symbol      string   `json:"symbol"`
	Side        string   `json:"side"`
	Qty         int64    `json:"qty"`
	EntryTS     string   `json:"entry_ts"`
	ExitTS      *string  `json:"exit_ts"`
	EntryPx     float64  `json:"entry_px"`
	ExitPx      *float64 `json:"exit_px"`
	RealizedPnL *float64 `json:"realized_pnl_usd"`
}

func (s *Server) handleBacktestTrades(w http.ResponseWriter, r *http.Request) {
	id, ok := backtestIDParam(w, r)
	if !ok {
		return
	}
	rows, err := s.runs.Trades(r.Context(), id)
	if errors.Is(err, runs.ErrRunNotFound) {
		writeError(w, http.StatusNotFound, CodeNotFound, fmt.Sprintf("backtest %d not found", id))
		return
	}
	if err != nil {
		internalError(w, s.log, "backtest trades", err)
		return
	}
	out := make([]tradeJSON, 0, len(rows))
	for _, t := range rows {
		tj := tradeJSON{
			ID:         t.ID,
			StrategyID: t.StrategyID,
			Symbol:     t.Symbol,
			Side:       t.Side,
			Qty:        int64(t.Qty),
			EntryTS:    t.EntryTS.UTC().Format(time.RFC3339Nano),
			EntryPx:    t.EntryPx.Float64(),
		}
		if t.ExitTS != nil {
			s := t.ExitTS.UTC().Format(time.RFC3339Nano)
			tj.ExitTS = &s
		}
		if t.ExitPx != nil {
			v := t.ExitPx.Float64()
			tj.ExitPx = &v
		}
		if t.RealizedPnL != nil {
			v := t.RealizedPnL.Float64()
			tj.RealizedPnL = &v
		}
		out = append(out, tj)
	}
	writeJSON(w, http.StatusOK, map[string]any{"trades": out})
}

// ---------------------------------------------------------------------------
// GET /api/v1/backtests/{id}/orders
// ---------------------------------------------------------------------------

func (s *Server) handleBacktestOrders(w http.ResponseWriter, r *http.Request) {
	id, ok := backtestIDParam(w, r)
	if !ok {
		return
	}
	raw, err := s.runs.Orders(r.Context(), id)
	if errors.Is(err, runs.ErrRunNotFound) {
		writeError(w, http.StatusNotFound, CodeNotFound, fmt.Sprintf("backtest %d not found", id))
		return
	}
	if err != nil {
		internalError(w, s.log, "backtest orders", err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if len(raw) == 0 {
		_, _ = w.Write([]byte("[]"))
		return
	}
	_, _ = w.Write(raw)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func backtestIDParam(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := chi.URLParam(r, "id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, CodeValidation,
			fmt.Sprintf("invalid backtest id %q (want a positive integer)", raw))
		return 0, false
	}
	return id, true
}

// backtestHasORB reports whether the Composition has an intraday_breakout (ORB)
// member — the ORB path trades a single intraday instrument, so it needs an
// orb_symbol (or exactly one ticker) at enqueue time.
func backtestHasORB(m *composition.Composition) bool {
	for _, mem := range m.Members {
		if mem.StrategyID == composition.StrategyIntradayBreakout {
			return true
		}
	}
	return false
}

func moneyFloatPtr(m *domain.Money) *float64 {
	if m == nil {
		return nil
	}
	v := m.Float64()
	return &v
}

func validRunStatus(s string) bool {
	switch s {
	case "RUNNING", "COMPLETE", "INTERRUPTED", "FAIL":
		return true
	}
	return false
}

func rawOrNull(raw []byte) any {
	if len(raw) == 0 {
		return nil
	}
	return jsonRaw(raw)
}

// jsonRaw embeds a pre-serialized JSON fragment in a writeJSON payload.
type jsonRaw []byte

func (j jsonRaw) MarshalJSON() ([]byte, error) {
	if len(j) == 0 {
		return []byte("null"), nil
	}
	return j, nil
}
