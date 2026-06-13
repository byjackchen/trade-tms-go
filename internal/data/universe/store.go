package universe

// store.go is the TimescaleDB-backed BarHistoryProvider + universe data
// tier (docs/spec/calendar-universe.md §2): the Python reference reads the
// parquet cache (data/sharadar_cache/reader.py); the Go port reads the
// tables the P0 importer fills, with byte-identical filter semantics.

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// Ticker-table filter values (tms.tickers.table_name; reader.py:89-96).
const (
	// TableSF1 limits to common stocks (the SEPA universe).
	TableSF1 = "SF1"
	// TableSFP limits to ETFs/funds.
	TableSFP = "SFP"
	// TableAny applies no table filter (reader's table=None).
	TableAny = ""
)

// Store reads universe inputs from PostgreSQL and persists snapshots.
// Safe for concurrent use (pgxpool is; the memoizing market-cap cache is
// mutex-guarded).
type Store struct {
	pool *pgxpool.Pool
}

// NewStore wraps a pgx pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// utcMidnight converts a calendar date to its UTC-midnight instant, the
// storage convention of tms.bars_daily.ts.
func utcMidnight(d calendar.Date) time.Time {
	return time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, time.UTC)
}

// ListUniverseForWindow returns tickers tradable at ANY point in
// [start, end] inclusive, sorted ascending — survivor-bias-free
// (reader.py:73-97 [MUST-MATCH]):
//
//	first_price_date IS NULL OR first_price_date <= end, AND
//	last_price_date  IS NULL OR last_price_date  >= start
//
// (the importer already mapped the reference's empty-string/NaT
// lastpricedate to NULL). table is TableSF1, TableSFP or TableAny.
func (s *Store) ListUniverseForWindow(ctx context.Context, start, end calendar.Date, table string) ([]string, error) {
	switch table {
	case TableAny, TableSF1, TableSFP:
	default:
		return nil, fmt.Errorf("universe: invalid table filter %q (want %q, %q or empty)", table, TableSF1, TableSFP)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT ticker
		FROM tms.tickers
		WHERE ($3 = '' OR table_name = $3)
		  AND (first_price_date IS NULL OR first_price_date <= $2::date)
		  AND (last_price_date IS NULL OR last_price_date >= $1::date)
		ORDER BY ticker ASC`,
		start.String(), end.String(), table)
	if err != nil {
		return nil, fmt.Errorf("universe: listing window universe [%s, %s] table=%q: %w", start, end, table, err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("universe: scanning window universe: %w", err)
	}
	return out, nil
}

// ListActiveTickers returns tickers tradable on asOf (both tables) —
// reader.list_active_tickers, i.e. the window filter with
// start = end = asOf (reader.py:57-71 [MUST-MATCH]).
func (s *Store) ListActiveTickers(ctx context.Context, asOf calendar.Date) ([]string, error) {
	return s.ListUniverseForWindow(ctx, asOf, asOf, TableAny)
}

// GetBars returns daily bars for one ticker in [start, end] inclusive,
// merged across SEP and SFP and sorted by date ascending (SEP before SFP on
// a tie, mirroring the reference's concat order; reader.py:99-127
// [MUST-MATCH]). Unknown tickers yield an empty slice, never an error.
// NULL prices/volumes (source NaN) come back as NaN.
func (s *Store) GetBars(ctx context.Context, ticker string, start, end calendar.Date) ([]OHLCV, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT ts, open, high, low, close, volume
		FROM tms.bars_daily
		WHERE ticker = $1 AND ts >= $2 AND ts <= $3
		ORDER BY ts ASC, source ASC`,
		ticker, utcMidnight(start), utcMidnight(end))
	if err != nil {
		return nil, fmt.Errorf("universe: querying bars %s [%s, %s]: %w", ticker, start, end, err)
	}
	defer rows.Close()

	var out []OHLCV
	for rows.Next() {
		var (
			ts                 time.Time
			o, h, l, c, volume *int64
		)
		if err := rows.Scan(&ts, &o, &h, &l, &c, &volume); err != nil {
			return nil, fmt.Errorf("universe: scanning bar for %s: %w", ticker, err)
		}
		bar := OHLCV{
			TS:    ts.UTC(),
			Open:  priceFloat(o),
			High:  priceFloat(h),
			Low:   priceFloat(l),
			Close: priceFloat(c),
		}
		if volume == nil {
			bar.Volume = math.NaN()
		} else {
			bar.Volume = float64(*volume)
		}
		out = append(out, bar)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("universe: reading bars for %s: %w", ticker, err)
	}
	return out, nil
}

// priceFloat converts a nullable 1e-4 fixed-point price to float64 with the
// exact decimal bridge (NULL -> NaN, matching the source frame's NaN).
func priceFloat(raw *int64) float64 {
	if raw == nil {
		return math.NaN()
	}
	return domain.Price(*raw).Float64()
}

// MarketCaps bulk-loads the latest market cap per ticker: the SF1 row with
// the greatest datekey across ALL dimensions, dimension DESC as the
// deterministic tie-break (byte-equivalent to the reference's stable
// sort_values("datekey").iloc[-1] over dimension-sorted parquet rows —
// multi_strategy_backtest.py:182-204 [MUST-MATCH], spec Open Q2 pinned).
// Tickers without SF1 rows, or whose latest row has NULL/NaN marketcap,
// map to 0.0 ("unknown; fails rule 8; sorts last").
func (s *Store) MarketCaps(ctx context.Context, tickers []string) (map[string]float64, error) {
	out := make(map[string]float64, len(tickers))
	for _, t := range tickers {
		out[t] = 0.0
	}
	if len(tickers) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ON (ticker) ticker, marketcap
		FROM tms.fundamentals_sf1
		WHERE ticker = ANY($1)
		ORDER BY ticker, datekey DESC, dimension DESC`,
		tickers)
	if err != nil {
		return nil, fmt.Errorf("universe: querying market caps: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			ticker string
			mc     *float64
		)
		if err := rows.Scan(&ticker, &mc); err != nil {
			return nil, fmt.Errorf("universe: scanning market cap: %w", err)
		}
		if mc != nil && !math.IsNaN(*mc) {
			out[ticker] = *mc
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("universe: reading market caps: %w", err)
	}
	return out, nil
}

// MarketCapLookup returns a memoized per-ticker lookup backed by single-row
// queries — the Go analog of _market_cap_lookup_factory for callers that
// cannot bulk-prefetch. Query errors are remembered and surfaced through
// Err() (the float contract has no error channel; the reference would have
// raised mid-assembly instead).
type CapCache struct {
	store *Store
	ctx   context.Context

	mu      sync.Mutex
	memo    map[string]float64
	firstEr error
}

// NewCapCache builds a memoizing lookup bound to ctx.
func (s *Store) NewCapCache(ctx context.Context) *CapCache {
	return &CapCache{store: s, ctx: ctx, memo: make(map[string]float64)}
}

// Lookup returns the memoized market cap, querying on first use; 0.0 for
// unknown tickers or after a query error (recorded in Err).
func (c *CapCache) Lookup(ticker string) float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if v, ok := c.memo[ticker]; ok {
		return v
	}
	caps, err := c.store.MarketCaps(c.ctx, []string{ticker})
	if err != nil {
		if c.firstEr == nil {
			c.firstEr = err
		}
		c.memo[ticker] = 0.0
		return 0.0
	}
	v := caps[ticker]
	c.memo[ticker] = v
	return v
}

// Seed pre-populates the memo (e.g. from a bulk MarketCaps call).
func (c *CapCache) Seed(caps map[string]float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for t, v := range caps {
		c.memo[t] = v
	}
}

// Err reports the first query error swallowed by Lookup, if any.
func (c *CapCache) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.firstEr
}
