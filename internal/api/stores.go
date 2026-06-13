package api

// stores.go declares the persistence seams of the HTTP layer. Handlers
// depend on these small interfaces so contract tests run against in-memory
// stubs; pgstore.go provides the production PostgreSQL implementation and
// *jobs.Queue satisfies JobQueue directly.

import (
	"context"
	"encoding/json"
	"time"

	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/data/universe"
	"github.com/byjackchen/trade-tms-go/internal/hyperopt/study"
	"github.com/byjackchen/trade-tms-go/internal/jobs"
	"github.com/byjackchen/trade-tms-go/internal/runs"
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

// RunsReader reads persisted backtest runs (satisfied by *runs.Store). It backs
// the GET /api/v1/backtests* endpoints (DB source of truth). All readers return
// runs.ErrRunNotFound for an unknown id.
type RunsReader interface {
	List(ctx context.Context, f runs.ListFilter) ([]runs.RunSummary, error)
	Get(ctx context.Context, id int64) (*runs.RunDetail, error)
	Equity(ctx context.Context, id int64, scope string) ([]runs.EquitySample, error)
	Trades(ctx context.Context, id int64) ([]runs.TradeRow, error)
	Orders(ctx context.Context, id int64) (json.RawMessage, error)
}

// StrategyReader resolves the active parameter document + schema for each of the
// four production strategies (SEPA / SectorRotation / Pairs / ORB). It backs
// GET /api/v1/strategies* and is satisfied by *strategyMetaReader (params.Loader
// + embedded baseline schema). ErrStrategyNotFound is returned for an unknown id.
type StrategyReader interface {
	// ListStrategies returns metadata for every registered strategy, in a
	// stable display order. Per-strategy resolution failures are folded into
	// the returned StrategyMeta.Error (a single bad params doc must not blank
	// the whole list).
	ListStrategies(ctx context.Context) ([]StrategyMeta, error)
	// GetStrategy resolves a single strategy by canonical id. It returns
	// ErrStrategyNotFound for an unknown id and surfaces a hard resolution
	// error (e.g. malformed promoted params) as a non-nil error.
	GetStrategy(ctx context.Context, id string) (*StrategyMeta, error)
}

// HyperoptReader reads persisted hyperopt studies/trials (satisfied by
// *study.Store). It backs the GET /api/v1/hyperopt* endpoints (DB source of
// truth). Get/Trials return study.ErrStudyNotFound for an unknown study_ts.
type HyperoptReader interface {
	List(ctx context.Context, strategy string, limit int) ([]study.StudyRow, error)
	Get(ctx context.Context, studyTS string) (*study.StudyRow, error)
	Trials(ctx context.Context, studyTS string) ([]study.TrialRow, error)
}

// HyperoptPromoter promotes a chosen trial's params to active_params with audit
// (satisfied by *study.Promoter). It backs POST /api/v1/hyperopt/{id}/promote.
type HyperoptPromoter interface {
	Promote(ctx context.Context, in study.PromoteInput) ([]study.PromotedStrategy, error)
}

// ParamSchema is one parameter's wire schema: default value + optional search
// bounds + type + description, in file order.
type ParamSchema struct {
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	Default     any      `json:"default"`
	SearchLow   *float64 `json:"search_low,omitempty"`
	SearchHigh  *float64 `json:"search_high,omitempty"`
	Description string   `json:"description,omitempty"`
}

// StrategyMeta is the resolved metadata + active params + schema for one
// strategy (the GET /api/v1/strategies element and GET /api/v1/strategies/{id}
// body).
type StrategyMeta struct {
	ID              string         `json:"id"`            // canonical loader id (sepa|sector_rotation|pairs|intraday_breakout)
	BacktestID      string         `json:"backtest_id"`   // strategy token the backtest enqueue accepts (sepa|sector_rotation|pairs|orb)
	Label           string         `json:"label"`         // short display label
	Description     string         `json:"description"`   // display.description
	CapitalPct      *float64       `json:"capital_pct"`   // allocation.capital_pct (nil = unallocated)
	Active          bool           `json:"active"`        // allocation.active (default true)
	ParamsSource    string         `json:"params_source"` // db|file|baseline
	SchemaVersion   int            `json:"schema_version"`
	ParametersCount int            `json:"parameters_count"`
	Parameters      []ParamSchema  `json:"parameters"`
	ActiveValues    map[string]any `json:"active_values"` // name -> resolved default value
	// Error is set (and the doc is otherwise zero-valued beyond id/label) when a
	// strategy's params failed to resolve; the list endpoint keeps the row.
	Error string `json:"error,omitempty"`

	// RawDoc is the full resolved parameter document (strategy, schema_version,
	// display, allocation, metadata, parameters{name:{default,...}}, constraints)
	// verbatim. It is NOT serialized inline; the detail handler emits it under a
	// top-level "payload" key so ground-truth tooling can read the canonical
	// document shape. Empty on the list path / on a resolution error.
	RawDoc json.RawMessage `json:"-"`
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
