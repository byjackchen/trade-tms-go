package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/data/sharadar"
	"github.com/byjackchen/trade-tms-go/internal/jobs"
)

// KindDataRefresh is the dispatch key served by DataRefresh.
const KindDataRefresh = "data.refresh"

// ErrAPISourceUnavailable is returned for source=api when no APISyncer is
// injected. In production that happens only when TMS_NASDAQ_DATA_LINK_API_KEY
// is unset: the Nasdaq Data Link catchup engine is built and wired
// (cmd/tms/worker.go -> handlers.SharadarAPISyncer), but it cannot run
// without an API key, so the worker degrades gracefully (parquet refreshes
// and every other job kind keep working) and source=api jobs fail with this
// actionable message instead of a half-run.
var ErrAPISourceUnavailable = errors.New(
	`data.refresh: source "api" is disabled because TMS_NASDAQ_DATA_LINK_API_KEY is not set on the worker ` +
		`(get a key at https://data.nasdaq.com/account/profile); use source "parquet" for cache backfills, ` +
		`or set the key and restart the worker`)

// APISyncRequest is the normalized input handed to an APISyncer.
type APISyncRequest struct {
	// Tables is the dataset selection in spec names (TICKERS/SEP/...);
	// empty = all.
	Tables []string
	// Tickers optionally restricts the sync; empty = all.
	Tickers []string
	// Since optionally floors the sync window (UTC midnight of the
	// requested calendar date); zero = let the syncer derive the
	// incremental window from dataset_sync / cache meta.
	Since time.Time
}

// APISyncer is the injection seam for the Nasdaq Data Link incremental
// sync engine. Implementations must honor ctx cancellation, report
// progress through report, and — per P1 locked decision (2) — derive any
// "today"/catchup-target defaults from the America/New_York trading date
// via internal/data/calendar (never local time, never bare UTC date math).
// The returned result must marshal to a JSON object (job result column).
type APISyncer interface {
	Sync(ctx context.Context, req APISyncRequest, report jobs.ProgressFn) (result any, err error)
}

// DataRefresh handles "data.refresh" jobs.
//
// Payload (JSON object; unknown fields rejected):
//
//	{
//	  "source":  "parquet" | "api",          // required
//	  "tables":  ["sep","sf1", ...],         // optional; default all
//	  "tickers": ["AAPL","MSFT", ...],       // optional; default all
//	  "since":   "YYYY-MM-DD"                // optional date floor
//	}
//
// source=parquet reuses the P0 importer over the configured cache dir
// (TMS_SHARADAR_CACHE_DIR — required, no discovery fallback per P1 locked
// decision (1)); source=api delegates to the injected APISyncer.
type DataRefresh struct {
	pool *pgxpool.Pool
	log  zerolog.Logger
	// cacheDir is the explicit Sharadar parquet cache root ("" = parquet
	// source unavailable; fail with a message naming the env var).
	cacheDir string
	// api is the optional incremental-sync engine (nil until the
	// data-sync phase wires it).
	api APISyncer
}

// NewDataRefresh builds the handler. cacheDir comes from
// config.SharadarCacheDir; api may be nil (see ErrAPISourceUnavailable).
func NewDataRefresh(pool *pgxpool.Pool, log zerolog.Logger, cacheDir string, api APISyncer) (*DataRefresh, error) {
	if pool == nil {
		return nil, errors.New("data.refresh: nil connection pool")
	}
	return &DataRefresh{
		pool:     pool,
		log:      log.With().Str("component", "data-refresh").Logger(),
		cacheDir: strings.TrimSpace(cacheDir),
		api:      api,
	}, nil
}

// Kind implements jobs.Handler.
func (h *DataRefresh) Kind() string { return KindDataRefresh }

// dataRefreshParams is the payload wire shape.
type dataRefreshParams struct {
	Source  string   `json:"source"`
	Tables  []string `json:"tables"`
	Tickers []string `json:"tickers"`
	Since   string   `json:"since"`
}

// parseParams validates the payload strictly: unknown fields, bad sources
// and malformed dates fail the job immediately rather than half-running.
func parseParams(payload json.RawMessage) (dataRefreshParams, time.Time, error) {
	var p dataRefreshParams
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return p, time.Time{}, fmt.Errorf("data.refresh: invalid payload: %w", err)
	}
	switch p.Source {
	case "parquet", "api":
	case "":
		return p, time.Time{}, errors.New(`data.refresh: payload field "source" is required ("parquet" or "api")`)
	default:
		return p, time.Time{}, fmt.Errorf(`data.refresh: unknown source %q (want "parquet" or "api")`, p.Source)
	}
	var since time.Time
	if p.Since != "" {
		d, err := calendar.ParseDate(p.Since)
		if err != nil {
			return p, time.Time{}, fmt.Errorf("data.refresh: invalid since %q (want YYYY-MM-DD): %w", p.Since, err)
		}
		// The importer compares against UTC-midnight key dates (spec
		// data-sharadar §key dates); "since" is a pure calendar date.
		since = time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, time.UTC)
	}
	return p, since, nil
}

// Run implements jobs.Handler.
func (h *DataRefresh) Run(ctx context.Context, job *jobs.Job, report jobs.ProgressFn) (any, error) {
	p, since, err := parseParams(job.Payload)
	if err != nil {
		return nil, err
	}
	log := h.log.With().Int64("job_id", job.ID).Str("source", p.Source).Logger()

	switch p.Source {
	case "api":
		if h.api == nil {
			return nil, ErrAPISourceUnavailable
		}
		log.Info().Strs("tables", p.Tables).Int("tickers", len(p.Tickers)).
			Time("since", since).Msg("starting api refresh")
		return h.api.Sync(ctx, APISyncRequest{Tables: p.Tables, Tickers: p.Tickers, Since: since}, report)

	case "parquet":
		return h.runParquet(ctx, log, p, since, report)
	}
	return nil, fmt.Errorf("data.refresh: unreachable source %q", p.Source) // guarded by parseParams
}

// runParquet executes the P0 importer over the configured cache root.
func (h *DataRefresh) runParquet(ctx context.Context, log zerolog.Logger, p dataRefreshParams, since time.Time, report jobs.ProgressFn) (any, error) {
	if h.cacheDir == "" {
		// P1 locked decision (1): explicit env only — no repo-root
		// discovery in the job path.
		return nil, errors.New("data.refresh: TMS_SHARADAR_CACHE_DIR is not set; the parquet source requires an explicit cache directory (no repo-root discovery in worker jobs)")
	}
	root, err := sharadar.ResolveCacheDir(h.cacheDir, "")
	if err != nil {
		return nil, fmt.Errorf("data.refresh: %w", err)
	}

	imp, err := sharadar.New(h.pool, log, sharadar.Options{
		CacheDir: root,
		Tables:   p.Tables,
		Tickers:  p.Tickers,
		Since:    since,
		OnProgress: func(ev sharadar.ProgressEvent) {
			// Forward importer progress into the job's progress column /
			// Redis. Cadence is already throttled by the importer
			// (ProgressEvery rows + one event per finished dataset).
			// Errors are informational by ProgressFn contract.
			if rerr := report(ctx, map[string]any{
				"phase":          "import",
				"dataset":        ev.Dataset,
				"file":           ev.File,
				"rows_read":      ev.RowsRead,
				"rows_skipped":   ev.RowsSkipped,
				"rows_failed":    ev.RowsFailed,
				"rows_upserted":  ev.RowsUpserted,
				"dataset_done":   ev.DatasetDone,
				"datasets_done":  ev.DatasetsDone,
				"datasets_total": ev.DatasetsTotal,
			}); rerr != nil && ctx.Err() == nil {
				log.Warn().Err(rerr).Msg("progress report failed; continuing import")
			}
		},
	})
	if err != nil {
		return nil, err
	}

	summary, runErr := imp.Run(ctx)
	if runErr != nil {
		return nil, runErr // ctx cancellation (cooperative cancel / drain)
	}

	result := summarize(summary)
	if summary.Failed() {
		// Mirror the CLI's --fail-on-errors default: captured per-file/
		// per-row errors fail the job loudly (reruns are safe — upserts
		// are idempotent). The summary still lands in the final progress
		// snapshot for diagnosis since a failed job stores no result.
		if _, rerr := json.Marshal(result); rerr == nil {
			_ = report(ctx, map[string]any{"phase": "done", "summary": result})
		}
		return nil, fmt.Errorf("data.refresh: import completed with captured errors: %s", errorDigest(summary))
	}
	return result, nil
}

// summarize renders the importer summary as the job result object.
func summarize(s *sharadar.Summary) map[string]any {
	tables := make([]map[string]any, 0, len(s.Tables))
	for _, t := range s.Tables {
		tables = append(tables, map[string]any{
			"dataset":       t.Dataset,
			"files":         t.FilesScanned,
			"files_failed":  t.FilesFailed,
			"rows_read":     t.RowsRead,
			"rows_skipped":  t.RowsSkipped,
			"rows_failed":   t.RowsFailed,
			"fields_nulled": t.FieldsNulled,
			"rows_upserted": t.RowsUpserted,
			"errors":        t.Errors,
			"errors_omit":   t.ErrorsOmit,
		})
	}
	return map[string]any{
		"source":     "parquet",
		"started":    s.Started.UTC().Format(time.RFC3339),
		"finished":   s.Finished.UTC().Format(time.RFC3339),
		"elapsed_ms": s.Finished.Sub(s.Started).Milliseconds(),
		"tables":     tables,
	}
}

// errorDigest compresses the summary's captured errors into one line for
// last_error.
func errorDigest(s *sharadar.Summary) string {
	var parts []string
	for _, t := range s.Tables {
		n := len(t.Errors) + t.ErrorsOmit
		if t.FilesFailed > 0 || n > 0 {
			parts = append(parts, fmt.Sprintf("%s: %d errors, %d failed files", t.Dataset, n, t.FilesFailed))
		}
	}
	if len(parts) == 0 {
		return "unknown"
	}
	return strings.Join(parts, "; ")
}
