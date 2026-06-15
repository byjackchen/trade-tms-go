package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/data/sharadar"
	"github.com/byjackchen/trade-tms-go/internal/jobs"
	"github.com/byjackchen/trade-tms-go/internal/jobs/handlers"
)

// Limits and defaults for the data endpoints (documented in docs/api.md).
const (
	// maxMissingDates caps the per-ticker gap drill-down list.
	maxMissingDates = 1000
	// maxWorstGaps caps the coverage summary's worst-offender list.
	maxWorstGaps = 10
	// maxRequestBody bounds JSON request bodies (64 KiB is generous for
	// ticker lists).
	maxRequestBody = 64 << 10

	defaultSearchLimit = 20
	maxSearchLimit     = 200
	defaultRunsLimit   = 50
	maxRunsLimit       = 500
)

// dedupe keys: at most one active refresh / rebuild at a time (both write
// the same tables; stacking them is never useful).
const (
	dedupeDataRefresh     = "data.refresh"
	dedupeUniverseRebuild = "universe.rebuild"
)

// ---------------------------------------------------------------------------
// GET /api/v1/data/coverage
// ---------------------------------------------------------------------------

// freshnessJSON reports how far a table trails the NYSE calendar.
type freshnessJSON struct {
	// LatestSession is the most recent NYSE session date not after "today"
	// in America/New_York (P1 locked decision (2): trading-date logic is
	// always the NY trading date via internal/data/calendar).
	LatestSession string `json:"latest_session"`
	// LagSessions counts NYSE sessions in (max_date, latest_session]; 0 =
	// fully fresh.
	LagSessions int `json:"lag_sessions"`
}

type tableCoverageJSON struct {
	Table     string         `json:"table"`
	Rows      int64          `json:"rows"`
	Tickers   int64          `json:"tickers"`
	MinDate   string         `json:"min_date,omitempty"`
	MaxDate   string         `json:"max_date,omitempty"`
	Freshness *freshnessJSON `json:"freshness,omitempty"`
	Gaps      *gapSummary    `json:"gaps,omitempty"`
}

type gapSummary struct {
	TickersScanned   int         `json:"tickers_scanned"`
	TickersWithGaps  int         `json:"tickers_with_gaps"`
	MissingDaysTotal int64       `json:"missing_days_total"`
	Worst            []tickerGap `json:"worst"`
}

type tickerGap struct {
	Ticker      string `json:"ticker"`
	First       string `json:"first"`
	Last        string `json:"last"`
	Bars        int64  `json:"bars"`
	Expected    int64  `json:"expected_sessions"`
	MissingDays int64  `json:"missing_days"`
}

// sessionIndex supports O(log n) "count NYSE sessions in [a, b]" queries
// over the precomputed session-date list.
type sessionIndex struct {
	dates []calendar.Date
}

func (s *Server) buildSessionIndex(upTo calendar.Date) (*sessionIndex, error) {
	sessions, err := s.cal.SessionsInRange(s.cal.MinDate(), upTo)
	if err != nil {
		return nil, err
	}
	idx := &sessionIndex{dates: make([]calendar.Date, len(sessions))}
	for i, sess := range sessions {
		idx.dates[i] = sess.Date
	}
	return idx, nil
}

// lowerBound returns the index of the first session date >= d.
func (x *sessionIndex) lowerBound(d calendar.Date) int {
	return sort.Search(len(x.dates), func(i int) bool { return !x.dates[i].Before(d) })
}

// count returns the number of sessions in [a, b] inclusive.
func (x *sessionIndex) count(a, b calendar.Date) int {
	if b.Before(a) {
		return 0
	}
	return x.lowerBound(b.AddDays(1)) - x.lowerBound(a)
}

// latestSession resolves the most recent NYSE session date <= "today" in
// America/New_York.
func (s *Server) latestSession() (calendar.Date, error) {
	today := calendar.DateOf(s.now(), s.cal.Location())
	sess, err := s.cal.PrevSession(today.AddDays(1))
	if err != nil {
		return calendar.Date{}, fmt.Errorf("api: resolving latest NYSE session for %s: %w", today, err)
	}
	return sess.Date, nil
}

// clampDate bounds d into [lo, hi].
func clampDate(d, lo, hi calendar.Date) calendar.Date {
	if d.Before(lo) {
		return lo
	}
	if d.After(hi) {
		return hi
	}
	return d
}

func (s *Server) handleCoverage(w http.ResponseWriter, r *http.Request) {
	if t := strings.TrimSpace(r.URL.Query().Get("ticker")); t != "" {
		s.handleCoverageTicker(w, r, strings.ToUpper(t))
		return
	}

	ctx := r.Context()
	latest, err := s.latestSession()
	if err != nil {
		internalError(w, s.log, "coverage: latest session", err)
		return
	}
	idx, err := s.buildSessionIndex(latest)
	if err != nil {
		internalError(w, s.log, "coverage: session index", err)
		return
	}

	tables, err := s.data.TableCoverage(ctx)
	if err != nil {
		internalError(w, s.log, "coverage: table stats", err)
		return
	}
	spans, err := s.data.BarSpans(ctx)
	if err != nil {
		internalError(w, s.log, "coverage: bar spans", err)
		return
	}

	gaps := s.summarizeGaps(spans, idx, latest)

	out := make([]tableCoverageJSON, 0, len(tables))
	for _, t := range tables {
		tj := tableCoverageJSON{Table: t.Table, Rows: t.Rows, Tickers: t.Tickers}
		if !t.MinDate.IsZero() {
			tj.MinDate = t.MinDate.String()
		}
		if !t.MaxDate.IsZero() {
			tj.MaxDate = t.MaxDate.String()
			lag := idx.count(t.MaxDate.AddDays(1), latest)
			tj.Freshness = &freshnessJSON{LatestSession: latest.String(), LagSessions: lag}
		}
		if t.Table == "bars_daily" {
			tj.Gaps = gaps
		}
		out = append(out, tj)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"latest_session": latest.String(),
		"generated_at":   s.now().UTC(),
		"tables":         out,
	})
}

// summarizeGaps compares each ticker's stored bar count with the NYSE
// session count over its own [first, last] span (clamped to the calendar's
// supported range — pre-2000 history is excluded from the expectation, so
// the missing-day counts are a lower bound there).
func (s *Server) summarizeGaps(spans []TickerSpan, idx *sessionIndex, latest calendar.Date) *gapSummary {
	sum := &gapSummary{TickersScanned: len(spans), Worst: []tickerGap{}}
	for _, sp := range spans {
		lo := clampDate(sp.First, s.cal.MinDate(), latest)
		hi := clampDate(sp.Last, s.cal.MinDate(), latest)
		expected := int64(idx.count(lo, hi))
		missing := expected - sp.Bars
		if missing <= 0 {
			continue
		}
		sum.TickersWithGaps++
		sum.MissingDaysTotal += missing
		sum.Worst = append(sum.Worst, tickerGap{
			Ticker:      sp.Ticker,
			First:       sp.First.String(),
			Last:        sp.Last.String(),
			Bars:        sp.Bars,
			Expected:    expected,
			MissingDays: missing,
		})
	}
	sort.Slice(sum.Worst, func(i, j int) bool {
		if sum.Worst[i].MissingDays != sum.Worst[j].MissingDays {
			return sum.Worst[i].MissingDays > sum.Worst[j].MissingDays
		}
		return sum.Worst[i].Ticker < sum.Worst[j].Ticker
	})
	if len(sum.Worst) > maxWorstGaps {
		sum.Worst = sum.Worst[:maxWorstGaps]
	}
	return sum
}

// handleCoverageTicker is the ?ticker= drill-down: the exact missing NYSE
// trading dates within the ticker's own bar span.
func (s *Server) handleCoverageTicker(w http.ResponseWriter, r *http.Request, ticker string) {
	ctx := r.Context()
	dates, err := s.data.BarDates(ctx, ticker)
	if err != nil {
		internalError(w, s.log, "coverage: bar dates", err)
		return
	}
	if len(dates) == 0 {
		exists, err := s.data.TickerExists(ctx, ticker)
		if err != nil {
			internalError(w, s.log, "coverage: ticker existence", err)
			return
		}
		if !exists {
			writeError(w, http.StatusNotFound, CodeNotFound, fmt.Sprintf("unknown ticker %q", ticker))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ticker": ticker, "bars": 0, "expected_sessions": 0,
			"missing": []string{}, "missing_truncated": false,
		})
		return
	}

	latest, err := s.latestSession()
	if err != nil {
		internalError(w, s.log, "coverage: latest session", err)
		return
	}
	idx, err := s.buildSessionIndex(latest)
	if err != nil {
		internalError(w, s.log, "coverage: session index", err)
		return
	}

	first, last := dates[0], dates[len(dates)-1]
	lo := clampDate(first, s.cal.MinDate(), latest)
	hi := clampDate(last, s.cal.MinDate(), latest)
	have := make(map[calendar.Date]struct{}, len(dates))
	for _, d := range dates {
		have[d] = struct{}{}
	}

	var (
		missing         []string
		truncated       bool
		expected, found int
	)
	loIdx, hiIdx := idx.lowerBound(lo), idx.lowerBound(hi.AddDays(1))
	for _, d := range idx.dates[loIdx:hiIdx] {
		expected++
		if _, ok := have[d]; ok {
			found++
			continue
		}
		if len(missing) >= maxMissingDates {
			truncated = true
			continue
		}
		missing = append(missing, d.String())
	}
	if missing == nil {
		missing = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ticker":            ticker,
		"first":             first.String(),
		"last":              last.String(),
		"bars":              len(dates),
		"expected_sessions": expected,
		"missing_days":      expected - found,
		"missing":           missing,
		"missing_truncated": truncated,
	})
}

// ---------------------------------------------------------------------------
// GET /api/v1/data/tickers?q=
// ---------------------------------------------------------------------------

func (s *Server) handleTickerSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeError(w, http.StatusBadRequest, CodeValidation, "query parameter q is required")
		return
	}
	limit, ok := parseLimit(w, r, defaultSearchLimit, maxSearchLimit)
	if !ok {
		return
	}
	results, err := s.data.SearchTickers(r.Context(), q, limit)
	if err != nil {
		internalError(w, s.log, "ticker search", err)
		return
	}
	if results == nil {
		results = []TickerMeta{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"query": q, "results": results})
}

// parseLimit reads ?limit= with a default and an upper bound, writing the
// 400 itself on bad input.
func parseLimit(w http.ResponseWriter, r *http.Request, def, max int) (int, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get("limit"))
	if raw == "" {
		return def, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > max {
		writeError(w, http.StatusBadRequest, CodeValidation,
			fmt.Sprintf("limit must be an integer in [1, %d], got %q", max, raw))
		return 0, false
	}
	return n, true
}

// ---------------------------------------------------------------------------
// GET /api/v1/data/sync-runs
// ---------------------------------------------------------------------------

func (s *Server) handleSyncRuns(w http.ResponseWriter, r *http.Request) {
	dataset := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("dataset")))
	if dataset != "" && !validDataset(dataset) {
		writeError(w, http.StatusBadRequest, CodeValidation,
			fmt.Sprintf("unknown dataset %q (want one of %s)", dataset, strings.Join(sharadar.DatasetOrder, ", ")))
		return
	}
	limit, ok := parseLimit(w, r, defaultRunsLimit, maxRunsLimit)
	if !ok {
		return
	}
	watermarks, err := s.data.SyncWatermarks(r.Context())
	if err != nil {
		internalError(w, s.log, "sync watermarks", err)
		return
	}
	runs, err := s.data.SyncRuns(r.Context(), dataset, limit)
	if err != nil {
		internalError(w, s.log, "sync runs", err)
		return
	}
	if watermarks == nil {
		watermarks = []SyncWatermark{}
	}
	if runs == nil {
		runs = []SyncRun{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"datasets": watermarks, "runs": runs})
}

func validDataset(name string) bool {
	for _, d := range sharadar.DatasetOrder {
		if name == d {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// POST /api/v1/data/refresh
// ---------------------------------------------------------------------------

// refreshRequest is the request body. Actor and MaxAttempts feed the queue
// (audit trail / retry budget); the rest becomes the data.refresh payload
// validated again by the worker-side handler.
type refreshRequest struct {
	Source      string   `json:"source"`
	Tables      []string `json:"tables"`
	Tickers     []string `json:"tickers"`
	Since       string   `json:"since"`
	Actor       string   `json:"actor"`
	MaxAttempts int32    `json:"max_attempts"`
}

// decodeStrictJSON decodes a single JSON object, rejecting unknown fields,
// trailing data and oversized bodies. An empty body decodes as {} (all
// endpoints have usable defaults or required-field validation downstream).
// The returned error text is safe for clients.
func decodeStrictJSON(r *http.Request, dst any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody+1))
	if err != nil {
		return fmt.Errorf("reading request body: %w", err)
	}
	if len(body) > maxRequestBody {
		return fmt.Errorf("request body exceeds %d bytes", maxRequestBody)
	}
	if len(bytes.TrimSpace(body)) == 0 {
		body = []byte("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("invalid JSON body: %v", err)
	}
	if dec.More() {
		return errors.New("invalid JSON body: trailing data after object")
	}
	return nil
}

func (s *Server) handleDataRefresh(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if err := decodeStrictJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeValidation, err.Error())
		return
	}

	switch req.Source {
	case "parquet", "api":
	case "":
		writeError(w, http.StatusBadRequest, CodeValidation, `field "source" is required ("parquet" or "api")`)
		return
	default:
		writeError(w, http.StatusBadRequest, CodeValidation,
			fmt.Sprintf(`unknown source %q (want "parquet" or "api")`, req.Source))
		return
	}

	tables := make([]string, 0, len(req.Tables))
	for _, t := range req.Tables {
		name := strings.ToUpper(strings.TrimSpace(t))
		if name == "" {
			continue
		}
		if !validDataset(name) {
			writeError(w, http.StatusBadRequest, CodeValidation,
				fmt.Sprintf("unknown table %q (want one of %s)", t, strings.Join(sharadar.DatasetOrder, ", ")))
			return
		}
		tables = append(tables, name)
	}

	tickers := make([]string, 0, len(req.Tickers))
	for _, t := range req.Tickers {
		name := strings.ToUpper(strings.TrimSpace(t))
		if name == "" {
			writeError(w, http.StatusBadRequest, CodeValidation, "tickers must not contain blank entries")
			return
		}
		tickers = append(tickers, name)
	}

	if req.Since != "" {
		if _, err := calendar.ParseDate(req.Since); err != nil {
			writeError(w, http.StatusBadRequest, CodeValidation,
				fmt.Sprintf("invalid since %q (want YYYY-MM-DD)", req.Since))
			return
		}
	}
	if req.MaxAttempts < 0 || req.MaxAttempts > 10 {
		writeError(w, http.StatusBadRequest, CodeValidation, "max_attempts must be in [0, 10] (0 = default 1)")
		return
	}

	payload := map[string]any{"source": req.Source}
	if len(tables) > 0 {
		payload["tables"] = tables
	}
	if len(tickers) > 0 {
		payload["tickers"] = tickers
	}
	if req.Since != "" {
		payload["since"] = req.Since
	}

	job, deduped, err := s.jobs.Enqueue(r.Context(), jobs.EnqueueParams{
		Kind:        handlers.KindDataRefresh,
		Payload:     payload,
		DedupeKey:   dedupeDataRefresh,
		MaxAttempts: req.MaxAttempts,
		Actor:       actorOrDefault(req.Actor),
	})
	if err != nil {
		internalError(w, s.log, "enqueue data.refresh", err)
		return
	}
	s.log.Info().Int64("job_id", job.ID).Str("source", req.Source).
		Bool("deduped", deduped).Str("actor", actorOrDefault(req.Actor)).
		Msg("data.refresh enqueued")
	writeJSON(w, http.StatusAccepted, map[string]any{"job": jobToJSON(job), "deduped": deduped})
}

// ---------------------------------------------------------------------------
// POST /api/v1/data/sync-now
// ---------------------------------------------------------------------------

// syncNowRequest is the (optional) request body: an actor label for the audit
// trail. The force itself takes no other input — it always runs the full
// daily pipeline for the current trading date.
type syncNowRequest struct {
	Actor string `json:"actor"`
}

// handleSyncNow forces the daily incremental-sync pipeline (data.refresh
// source=api -> eod.refresh) immediately via the SyncForcer, bypassing the
// scheduler's configured fire time. Idempotent through the per-trading-date
// ledger slot: a day already enqueued returns deduped=true with no new jobs.
func (s *Server) handleSyncNow(w http.ResponseWriter, r *http.Request) {
	if s.sync == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable",
			"sync-now is not configured (the scheduler force path is not wired)")
		return
	}
	var req syncNowRequest
	if err := decodeStrictJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeValidation, err.Error())
		return
	}
	actor := actorOrDefault(req.Actor)
	res, err := s.sync.SyncNow(r.Context(), actor)
	if err != nil {
		internalError(w, s.log, "sync-now", err)
		return
	}
	s.log.Info().Str("trading_date", res.TradingDate).Bool("forced", res.Forced).
		Int64("data_job_id", res.DataJobID).Int64("eod_job_id", res.EODJobID).
		Str("actor", actor).Msg("data sync-now")
	writeJSON(w, http.StatusAccepted, map[string]any{
		"trading_date": res.TradingDate,
		"forced":       res.Forced,
		"deduped":      !res.Forced,
		"data_job_id":  res.DataJobID,
		"eod_job_id":   res.EODJobID,
	})
}

// actorOrDefault stamps API-submitted work with the "api:" prefix so the
// audit trail distinguishes HTTP actors from CLI/system ones.
func actorOrDefault(actor string) string {
	actor = strings.TrimSpace(actor)
	if actor == "" {
		return "api"
	}
	return "api:" + actor
}
