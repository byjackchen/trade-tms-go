package apistore

// pgstore.go is the production api.DataStore over PostgreSQL/TimescaleDB.
// Date columns are read as text ("YYYY-MM-DD"); bars_daily.ts is stored at
// UTC midnight, so every ts -> date conversion pins AT TIME ZONE 'UTC'
// rather than trusting the session TimeZone setting.
//
// This package holds the concrete pgx-backed implementations of the
// api.stores.go interface seams, keeping the internal/api HTTP layer free of
// any jackc/pgx dependency (matching the codebase's dedicated store-package
// pattern: runs.Store, universe.Store, study.Store, …).

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/byjackchen/trade-tms-go/internal/api"
	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
)

// PGStore implements api.DataStore over a pgx pool.
type PGStore struct {
	pool *pgxpool.Pool
}

// NewPGStore wraps a pool.
func NewPGStore(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

// parseOptionalDate converts "" to the zero Date and anything else through
// calendar.ParseDate.
func parseOptionalDate(s string) (calendar.Date, error) {
	if s == "" {
		return calendar.Date{}, nil
	}
	return calendar.ParseDate(s)
}

// TableCoverage implements api.DataStore. One aggregate query per table; the
// four tables are fixed (the P0 market-data schema).
func (s *PGStore) TableCoverage(ctx context.Context) ([]api.TableCoverage, error) {
	type spec struct{ table, sql string }
	specs := []spec{
		{"tickers", `
			SELECT count(*), count(DISTINCT ticker),
			       COALESCE(min(first_price_date)::text, ''),
			       COALESCE(max(last_price_date)::text, '')
			FROM tms.tickers`},
		{"bars_daily", `
			SELECT count(*), count(DISTINCT ticker),
			       COALESCE(min(ts AT TIME ZONE 'UTC')::date::text, ''),
			       COALESCE(max(ts AT TIME ZONE 'UTC')::date::text, '')
			FROM tms.bars_daily`},
		{"fundamentals_sf1", `
			SELECT count(*), count(DISTINCT ticker),
			       COALESCE(min(datekey)::text, ''),
			       COALESCE(max(datekey)::text, '')
			FROM tms.fundamentals_sf1`},
		{"events", `
			SELECT count(*), count(DISTINCT ticker),
			       COALESCE(min(event_date)::text, ''),
			       COALESCE(max(event_date)::text, '')
			FROM tms.events`},
	}
	out := make([]api.TableCoverage, 0, len(specs))
	for _, sp := range specs {
		var (
			tc       = api.TableCoverage{Table: sp.table}
			min, max string
		)
		if err := s.pool.QueryRow(ctx, sp.sql).Scan(&tc.Rows, &tc.Tickers, &min, &max); err != nil {
			return nil, fmt.Errorf("api: coverage query for %s: %w", sp.table, err)
		}
		var err error
		if tc.MinDate, err = parseOptionalDate(min); err != nil {
			return nil, fmt.Errorf("api: coverage min date for %s: %w", sp.table, err)
		}
		if tc.MaxDate, err = parseOptionalDate(max); err != nil {
			return nil, fmt.Errorf("api: coverage max date for %s: %w", sp.table, err)
		}
		out = append(out, tc)
	}
	return out, nil
}

// BarSpans implements api.DataStore.
func (s *PGStore) BarSpans(ctx context.Context) ([]api.TickerSpan, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT ticker, count(*),
		       min(ts AT TIME ZONE 'UTC')::date::text,
		       max(ts AT TIME ZONE 'UTC')::date::text
		FROM tms.bars_daily
		GROUP BY ticker
		ORDER BY ticker`)
	if err != nil {
		return nil, fmt.Errorf("api: querying bar spans: %w", err)
	}
	defer rows.Close()
	var out []api.TickerSpan
	for rows.Next() {
		var (
			sp          api.TickerSpan
			first, last string
		)
		if err := rows.Scan(&sp.Ticker, &sp.Bars, &first, &last); err != nil {
			return nil, fmt.Errorf("api: scanning bar span: %w", err)
		}
		if sp.First, err = calendar.ParseDate(first); err != nil {
			return nil, fmt.Errorf("api: bar span first date: %w", err)
		}
		if sp.Last, err = calendar.ParseDate(last); err != nil {
			return nil, fmt.Errorf("api: bar span last date: %w", err)
		}
		out = append(out, sp)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("api: reading bar spans: %w", err)
	}
	return out, nil
}

// BarDates implements api.DataStore.
func (s *PGStore) BarDates(ctx context.Context, ticker string) ([]calendar.Date, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT (ts AT TIME ZONE 'UTC')::date::text
		FROM tms.bars_daily
		WHERE ticker = $1
		ORDER BY 1`, ticker)
	if err != nil {
		return nil, fmt.Errorf("api: querying bar dates for %s: %w", ticker, err)
	}
	defer rows.Close()
	var out []calendar.Date
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("api: scanning bar date for %s: %w", ticker, err)
		}
		d, err := calendar.ParseDate(raw)
		if err != nil {
			return nil, fmt.Errorf("api: bar date for %s: %w", ticker, err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("api: reading bar dates for %s: %w", ticker, err)
	}
	return out, nil
}

// TickerExists implements api.DataStore.
func (s *PGStore) TickerExists(ctx context.Context, ticker string) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM tms.tickers WHERE ticker = $1)`, ticker).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("api: ticker existence check for %s: %w", ticker, err)
	}
	return exists, nil
}

// escapeLike neutralizes LIKE/ILIKE metacharacters in user input.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// SearchTickers implements api.DataStore: ticker-prefix OR name-substring,
// case-insensitive; exact ticker match ranks first, then prefix matches,
// then alphabetical.
func (s *PGStore) SearchTickers(ctx context.Context, q string, limit int) ([]api.TickerMeta, error) {
	esc := escapeLike(q)
	rows, err := s.pool.Query(ctx, `
		SELECT ticker, COALESCE(name, ''), COALESCE(exchange, ''), is_delisted,
		       COALESCE(category, ''), COALESCE(sector, ''), COALESCE(industry, ''),
		       table_name,
		       COALESCE(first_price_date::text, ''),
		       COALESCE(last_price_date::text, ''),
		       COALESCE(delist_date::text, '')
		FROM tms.tickers
		WHERE ticker ILIKE $1 || '%' OR name ILIKE '%' || $1 || '%'
		ORDER BY (upper(ticker) = upper($2)) DESC,
		         (ticker ILIKE $1 || '%') DESC,
		         ticker
		LIMIT $3`, esc, q, limit)
	if err != nil {
		return nil, fmt.Errorf("api: searching tickers %q: %w", q, err)
	}
	defer rows.Close()
	var out []api.TickerMeta
	for rows.Next() {
		var m api.TickerMeta
		if err := rows.Scan(&m.Ticker, &m.Name, &m.Exchange, &m.IsDelisted,
			&m.Category, &m.Sector, &m.Industry, &m.Table,
			&m.FirstPriceDate, &m.LastPriceDate, &m.DelistDate); err != nil {
			return nil, fmt.Errorf("api: scanning ticker search row: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("api: reading ticker search rows: %w", err)
	}
	return out, nil
}

// SyncWatermarks implements api.DataStore.
func (s *PGStore) SyncWatermarks(ctx context.Context) ([]api.SyncWatermark, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT dataset, last_sync, row_count, schema_version, updated_at
		FROM tms.dataset_sync
		ORDER BY dataset`)
	if err != nil {
		return nil, fmt.Errorf("api: querying sync watermarks: %w", err)
	}
	defer rows.Close()
	var out []api.SyncWatermark
	for rows.Next() {
		var (
			w        api.SyncWatermark
			lastSync *time.Time
		)
		if err := rows.Scan(&w.Dataset, &lastSync, &w.RowCount, &w.SchemaVersion, &w.UpdatedAt); err != nil {
			return nil, fmt.Errorf("api: scanning sync watermark: %w", err)
		}
		w.LastSync = lastSync
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("api: reading sync watermarks: %w", err)
	}
	return out, nil
}

// QueueDepth implements api.SystemReader: one grouped count over tms.jobs for the
// two non-terminal states (queued / running). A single round trip.
func (s *PGStore) QueueDepth(ctx context.Context) (queued, running int, err error) {
	err = s.pool.QueryRow(ctx, `
		SELECT
		  count(*) FILTER (WHERE status = 'queued')  AS queued,
		  count(*) FILTER (WHERE status = 'running') AS running
		FROM tms.jobs`).Scan(&queued, &running)
	if err != nil {
		return 0, 0, fmt.Errorf("api: querying job-queue depth: %w", err)
	}
	return queued, running, nil
}

// ActiveSessions implements api.SystemReader: the count of live sessions in the
// RUNNING state (the unique partial index guarantees at most one per trader,
// but a multi-trader deployment can have several).
func (s *PGStore) ActiveSessions(ctx context.Context) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM tms.sessions WHERE status = 'RUNNING'`).Scan(&n); err != nil {
		return 0, fmt.Errorf("api: counting active sessions: %w", err)
	}
	return n, nil
}

// DataFreshness implements api.SystemReader: the most recent stored daily-bar date
// (the market-data horizon) and the most recent dataset-sync wall-clock time
// (when the data was last refreshed). Both are "" / nil when no data exists.
func (s *PGStore) DataFreshness(ctx context.Context) (latestBarDate string, lastSyncAt *time.Time, err error) {
	if err = s.pool.QueryRow(ctx, `
		SELECT COALESCE(max(ts AT TIME ZONE 'UTC')::date::text, '')
		FROM tms.bars_daily`).Scan(&latestBarDate); err != nil {
		return "", nil, fmt.Errorf("api: querying latest bar date: %w", err)
	}
	if err = s.pool.QueryRow(ctx,
		`SELECT max(last_sync) FROM tms.dataset_sync`).Scan(&lastSyncAt); err != nil {
		return "", nil, fmt.Errorf("api: querying last sync time: %w", err)
	}
	return latestBarDate, lastSyncAt, nil
}

// SyncRuns implements api.DataStore.
func (s *PGStore) SyncRuns(ctx context.Context, dataset string, limit int) ([]api.SyncRun, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, dataset, kind, started_at, finished_at, rows_added, status,
		       COALESCE(error, '')
		FROM tms.dataset_sync_runs
		WHERE ($1 = '' OR dataset = $1)
		ORDER BY id DESC
		LIMIT $2`, dataset, limit)
	if err != nil {
		return nil, fmt.Errorf("api: querying sync runs: %w", err)
	}
	defer rows.Close()
	var out []api.SyncRun
	for rows.Next() {
		var r api.SyncRun
		if err := rows.Scan(&r.ID, &r.Dataset, &r.Kind, &r.StartedAt, &r.FinishedAt,
			&r.RowsAdded, &r.Status, &r.Error); err != nil {
			return nil, fmt.Errorf("api: scanning sync run: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("api: reading sync runs: %w", err)
	}
	return out, nil
}

// compile-time checks: PGStore is both the api.DataStore and the api.SystemReader.
var (
	_ api.DataStore    = (*PGStore)(nil)
	_ api.SystemReader = (*PGStore)(nil)
)
