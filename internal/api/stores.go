package api

// stores.go declares the persistence seams of the HTTP layer. Handlers
// depend on these small interfaces so contract tests run against in-memory
// stubs; pgstore.go provides the production PostgreSQL implementation and
// *jobs.Queue satisfies JobQueue directly.

import (
	"context"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/data/universe"
	"github.com/byjackchen/trade-tms-go/internal/jobs"
)

// JobQueue is the queue surface the API needs (satisfied by *jobs.Queue).
type JobQueue interface {
	Enqueue(ctx context.Context, p jobs.EnqueueParams) (job *jobs.Job, deduped bool, err error)
	Get(ctx context.Context, jobID int64) (*jobs.Job, error)
	List(ctx context.Context, f jobs.ListFilter) ([]*jobs.Job, error)
	Cancel(ctx context.Context, jobID int64, actor, reason string) (jobs.CancelOutcome, *jobs.Job, error)
}

// UniverseReader reads persisted universe snapshots (satisfied by
// *universe.Store). kind "" matches any kind.
type UniverseReader interface {
	LatestSnapshot(ctx context.Context, kind string) (*universe.Snapshot, error)
}

// TableCoverage is one market-data table's aggregate coverage. Zero dates
// mean "table has no date column values" (empty table).
type TableCoverage struct {
	Table   string
	Rows    int64
	Tickers int64
	MinDate calendar.Date
	MaxDate calendar.Date
}

// TickerSpan is one ticker's daily-bar span (gap-detection input).
type TickerSpan struct {
	Ticker string
	Bars   int64
	First  calendar.Date
	Last   calendar.Date
}

// TickerMeta is tms.tickers metadata for the search endpoint. Optional
// text/date columns are "" when NULL.
type TickerMeta struct {
	Ticker         string `json:"ticker"`
	Name           string `json:"name"`
	Exchange       string `json:"exchange"`
	IsDelisted     bool   `json:"is_delisted"`
	Category       string `json:"category"`
	Sector         string `json:"sector"`
	Industry       string `json:"industry"`
	Table          string `json:"table"`
	FirstPriceDate string `json:"first_price_date"`
	LastPriceDate  string `json:"last_price_date"`
	DelistDate     string `json:"delist_date"`
}

// SyncWatermark mirrors one tms.dataset_sync row (CacheMeta parity).
type SyncWatermark struct {
	Dataset       string     `json:"dataset"`
	LastSync      *time.Time `json:"last_sync"`
	RowCount      int64      `json:"row_count"`
	SchemaVersion int        `json:"schema_version"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// SyncRun mirrors one tms.dataset_sync_runs audit row.
type SyncRun struct {
	ID         int64      `json:"id"`
	Dataset    string     `json:"dataset"`
	Kind       string     `json:"kind"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
	RowsAdded  int64      `json:"rows_added"`
	Status     string     `json:"status"`
	Error      string     `json:"error,omitempty"`
}

// DataStore reads market-data coverage and sync bookkeeping.
type DataStore interface {
	// TableCoverage returns aggregate stats for the market-data tables
	// (tickers, bars_daily, fundamentals_sf1, events).
	TableCoverage(ctx context.Context) ([]TableCoverage, error)
	// BarSpans returns per-ticker daily-bar spans for gap detection.
	BarSpans(ctx context.Context) ([]TickerSpan, error)
	// BarDates returns the distinct trading dates stored for one ticker,
	// ascending; empty when the ticker has no bars.
	BarDates(ctx context.Context, ticker string) ([]calendar.Date, error)
	// TickerExists reports whether the ticker is in tms.tickers.
	TickerExists(ctx context.Context, ticker string) (bool, error)
	// SearchTickers searches by ticker prefix or name substring
	// (case-insensitive), exact ticker matches first.
	SearchTickers(ctx context.Context, q string, limit int) ([]TickerMeta, error)
	// SyncWatermarks returns the per-dataset tms.dataset_sync rows.
	SyncWatermarks(ctx context.Context) ([]SyncWatermark, error)
	// SyncRuns returns tms.dataset_sync_runs newest-first, optionally
	// filtered by dataset ("" = all).
	SyncRuns(ctx context.Context, dataset string, limit int) ([]SyncRun, error)
}
