package handlers

// eodrefresh.go is the "eod.refresh" job handler: it runs the idempotent EOD
// engine-replay (internal/runner.EOD) for an as_of date, upserting each
// strategy's evaluate_intent into tms.signals (idempotent on
// (strategy_id, symbol, as_of)) and publishing to Redis. Re-running the job for
// the same as_of OVERWRITES the rows (no dupes) — the idempotency contract.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"

	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/jobs"
	"github.com/byjackchen/trade-tms-go/internal/publish"
	"github.com/byjackchen/trade-tms-go/internal/runner"
)

// KindEODRefresh is the dispatch key served by EODRefresh.
const KindEODRefresh = "eod.refresh"

// EODRefresh handles "eod.refresh" jobs.
//
// Payload (JSON object; unknown fields rejected):
//
//	{
//	  "as_of":            "YYYY-MM-DD",   // required (the refresh date)
//	  "strategy":         "multi",        // sepa|sector_rotation|pairs|orb|multi; default multi
//	  "tickers":          ["AAPL", ...],  // SEPA stock universe (sepa/multi)
//	  "orb_symbol":       "AAPL",          // orb path
//	  "starting_balance": 100000.0,        // informational health NAV; default 100000
//	  "trader_id":        "SIGNAL-001",    // Redis namespace for published updates ("" -> PG only)
//	  "window_days":      400              // replay window calendar days; default 400
//	}
type EODRefresh struct {
	eod       *runner.EOD
	rdb       *redis.Client
	paramsDir string
	log       zerolog.Logger
}

// NewEODRefresh builds the handler. rdb may be nil (PG-only refresh).
func NewEODRefresh(pool *pgxpool.Pool, rdb *redis.Client, paramsDir string, log zerolog.Logger) (*EODRefresh, error) {
	if pool == nil {
		return nil, errors.New("eod.refresh: nil connection pool")
	}
	return &EODRefresh{
		eod:       runner.NewEOD(pool, paramsDir, log),
		rdb:       rdb,
		paramsDir: paramsDir,
		log:       log.With().Str("component", "eod-refresh-job").Logger(),
	}, nil
}

// Kind implements jobs.Handler.
func (h *EODRefresh) Kind() string { return KindEODRefresh }

// eodParams is the payload wire shape.
type eodParams struct {
	AsOf            string   `json:"as_of"`
	Strategy        string   `json:"strategy"`
	Tickers         []string `json:"tickers"`
	ORBSymbol       string   `json:"orb_symbol"`
	StartingBalance *float64 `json:"starting_balance"`
	TraderID        string   `json:"trader_id"`
	WindowDays      int      `json:"window_days"`
}

// Run implements jobs.Handler.
func (h *EODRefresh) Run(ctx context.Context, job *jobs.Job, report jobs.ProgressFn) (any, error) {
	p, err := parseEODParams(job.Payload)
	if err != nil {
		return nil, err
	}
	asOf, err := calendar.ParseDate(p.AsOf)
	if err != nil {
		return nil, fmt.Errorf("eod.refresh: invalid as_of %q (want YYYY-MM-DD): %w", p.AsOf, err)
	}
	startBal := 100000.0
	if p.StartingBalance != nil {
		startBal = *p.StartingBalance
	}

	if rerr := report(ctx, map[string]any{"phase": "assemble", "as_of": p.AsOf}); rerr != nil && ctx.Err() == nil {
		h.log.Warn().Err(rerr).Msg("progress report failed; continuing")
	}

	// Redis publisher (best-effort transport; nil rdb / empty trader -> no-op).
	var publisher *publish.Publisher
	if h.rdb != nil && p.TraderID != "" {
		publisher = publish.NewPublisher(h.rdb, publish.Options{TraderID: p.TraderID, Logger: h.log})
	}

	rep, err := h.eod.RunRefresh(ctx, runner.EODConfig{
		AsOf:               asOf,
		Strategy:           p.Strategy,
		Tickers:            p.Tickers,
		ORBSymbol:          p.ORBSymbol,
		StartingBalance:    startBal,
		WindowCalendarDays: p.WindowDays,
		TraderID:           p.TraderID,
	}, publisher)
	if err != nil {
		return nil, fmt.Errorf("eod.refresh: %w", err)
	}

	if rerr := report(ctx, map[string]any{
		"phase": "done", "as_of": rep.AsOf, "intent_rows": rep.SignalRows, "bars": rep.BarsReplayed,
	}); rerr != nil && ctx.Err() == nil {
		h.log.Warn().Err(rerr).Msg("done progress report failed")
	}

	// Return the RefreshReport as the job result (a JSON object).
	b, err := json.Marshal(rep)
	if err != nil {
		return nil, fmt.Errorf("eod.refresh: marshal report: %w", err)
	}
	var result map[string]any
	if err := json.Unmarshal(b, &result); err != nil {
		return nil, fmt.Errorf("eod.refresh: report to map: %w", err)
	}
	return result, nil
}

// parseEODParams strictly decodes the payload (unknown fields rejected).
func parseEODParams(payload json.RawMessage) (eodParams, error) {
	var p eodParams
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return p, fmt.Errorf("eod.refresh: invalid payload: %w", err)
	}
	if p.AsOf == "" {
		return p, errors.New("eod.refresh: payload requires \"as_of\" (YYYY-MM-DD)")
	}
	return p, nil
}
