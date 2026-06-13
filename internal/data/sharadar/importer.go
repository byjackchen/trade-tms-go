package sharadar

// importer.go orchestrates the cache -> TimescaleDB bulk load. See doc.go
// for the upsert design rationale and the error policy.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
)

// Defaults for the knobs exposed as CLI flags.
const (
	// DefaultBatchSize is the staged-rows-per-merge flush bound. 20k rows of
	// the widest table (SF1, 113 staged columns) is ~50 MB of []any cells —
	// memory-bounded regardless of source file size.
	DefaultBatchSize = 20_000
	// DefaultProgressEvery is the progress-log cadence in source rows.
	DefaultProgressEvery = 100_000
	// maxCapturedErrors bounds per-dataset error capture so a systematically
	// corrupt source cannot balloon the summary.
	maxCapturedErrors = 20
)

// Options configures one import run.
type Options struct {
	// CacheDir is the resolved cache root (see ResolveCacheDir).
	CacheDir string
	// Tables is the dataset selection in spec names (TICKERS/SEP/SFP/SF1/
	// EVENTS); empty means all, imported in DatasetOrder.
	Tables []string
	// Tickers restricts the import to these symbols; empty means all.
	Tickers []string
	// Since drops rows whose key date (SEP/SFP/EVENTS: date, SF1: datekey)
	// is before this UTC date; zero means no filter. TICKERS is unaffected.
	Since time.Time
	// BatchSize is the staging flush bound in rows (<=0: DefaultBatchSize).
	BatchSize int
	// ProgressEvery logs progress every N source rows (<=0: default).
	ProgressEvery int64
	// OnProgress, when non-nil, receives progress callbacks at the same
	// cadence as the progress log lines (every ProgressEvery source rows)
	// plus one DatasetDone event per completed dataset. Callbacks run
	// synchronously on the import goroutine — keep them fast; consumers
	// (e.g. the data.refresh job handler) forward them to the jobs
	// progress column / Redis.
	OnProgress func(ProgressEvent)
}

// ProgressEvent is one OnProgress callback payload.
type ProgressEvent struct {
	// Dataset is the spec dataset name (TICKERS/SEP/SFP/SF1/EVENTS).
	Dataset string
	// File is the source file currently being scanned ("" on DatasetDone).
	File string
	// Rows* mirror the running TableStats counters for this dataset.
	RowsRead     int64
	RowsSkipped  int64
	RowsFailed   int64
	RowsUpserted int64
	// DatasetDone marks the completion callback for Dataset.
	DatasetDone bool
	// DatasetsDone / DatasetsTotal locate the run within its dataset list
	// (done counts fully completed datasets).
	DatasetsDone  int
	DatasetsTotal int
}

// TableStats is the per-dataset outcome.
type TableStats struct {
	Dataset      string
	FilesScanned int
	FilesFailed  int
	RowsRead     int64 // rows seen in source files
	RowsSkipped  int64 // dropped by --tickers / --since filters
	RowsFailed   int64 // unconvertible rows (missing keys, bad enums)
	FieldsNulled int64 // unrepresentable scalar fields stored as NULL
	RowsUpserted int64 // rows inserted or updated in the target table
	Errors       []string
	ErrorsOmit   int // captured-error overflow beyond maxCapturedErrors
}

func (st *TableStats) recordError(msg string) {
	if len(st.Errors) < maxCapturedErrors {
		st.Errors = append(st.Errors, msg)
		return
	}
	st.ErrorsOmit++
}

// Summary is the whole-run outcome.
type Summary struct {
	Started  time.Time
	Finished time.Time
	Tables   []*TableStats
}

// String renders the final rows-per-table summary.
func (s *Summary) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-8s %8s %6s %12s %10s %8s %8s %10s\n",
		"dataset", "files", "failed", "rows", "skipped", "badrows", "nulled", "upserted")
	for _, t := range s.Tables {
		fmt.Fprintf(&b, "%-8s %8d %6d %12d %10d %8d %8d %10d\n",
			t.Dataset, t.FilesScanned, t.FilesFailed, t.RowsRead, t.RowsSkipped,
			t.RowsFailed, t.FieldsNulled, t.RowsUpserted)
	}
	fmt.Fprintf(&b, "elapsed: %s\n", s.Finished.Sub(s.Started).Round(time.Millisecond))
	for _, t := range s.Tables {
		for _, e := range t.Errors {
			fmt.Fprintf(&b, "error [%s]: %s\n", t.Dataset, e)
		}
		if t.ErrorsOmit > 0 {
			fmt.Fprintf(&b, "error [%s]: ... and %d more\n", t.Dataset, t.ErrorsOmit)
		}
	}
	return b.String()
}

// Failed reports whether anything in the run went wrong (file failures or
// captured errors); callers decide whether that is fatal.
func (s *Summary) Failed() bool {
	for _, t := range s.Tables {
		if t.FilesFailed > 0 || len(t.Errors) > 0 || t.ErrorsOmit > 0 {
			return true
		}
	}
	return false
}

// Importer loads the Sharadar parquet cache into postgres.
type Importer struct {
	pool      *pgxpool.Pool
	log       zerolog.Logger
	opts      Options
	tickerSet map[string]struct{} // nil = no filter
	datasets  []string
	// datasetsDone counts fully completed datasets within Run (drives the
	// DatasetsDone field of OnProgress events; single goroutine, no lock).
	datasetsDone int
}

// New validates options and builds an Importer.
func New(pool *pgxpool.Pool, log zerolog.Logger, opts Options) (*Importer, error) {
	if pool == nil {
		return nil, errors.New("sharadar: nil connection pool")
	}
	if opts.CacheDir == "" {
		return nil, errors.New("sharadar: empty cache dir")
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = DefaultBatchSize
	}
	if opts.ProgressEvery <= 0 {
		opts.ProgressEvery = DefaultProgressEvery
	}

	datasets, err := normalizeTables(opts.Tables)
	if err != nil {
		return nil, err
	}

	var tickerSet map[string]struct{}
	if len(opts.Tickers) > 0 {
		tickerSet = make(map[string]struct{}, len(opts.Tickers))
		for _, t := range opts.Tickers {
			t = strings.ToUpper(strings.TrimSpace(t))
			if t != "" {
				tickerSet[t] = struct{}{}
			}
		}
		if len(tickerSet) == 0 {
			return nil, errors.New("sharadar: --tickers given but empty after normalization")
		}
	}

	return &Importer{
		pool:      pool,
		log:       log.With().Str("component", "sharadar-import").Logger(),
		opts:      opts,
		tickerSet: tickerSet,
		datasets:  datasets,
	}, nil
}

// normalizeTables maps a user table selection onto DatasetOrder.
func normalizeTables(tables []string) ([]string, error) {
	if len(tables) == 0 {
		return append([]string(nil), DatasetOrder...), nil
	}
	want := make(map[string]struct{}, len(tables))
	for _, t := range tables {
		name := strings.ToUpper(strings.TrimSpace(t))
		if name == "" {
			continue
		}
		if name == "ALL" {
			return append([]string(nil), DatasetOrder...), nil
		}
		valid := false
		for _, d := range DatasetOrder {
			if name == d {
				valid = true
				break
			}
		}
		if !valid {
			return nil, fmt.Errorf("sharadar: unknown table %q (want %s or all)", t, strings.Join(DatasetOrder, "|"))
		}
		want[name] = struct{}{}
	}
	out := make([]string, 0, len(want))
	for _, d := range DatasetOrder {
		if _, ok := want[d]; ok {
			out = append(out, d)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("sharadar: empty table selection")
	}
	return out, nil
}

func isCtxErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// Run executes the import. Dataset order follows the Python bootstrap
// (TICKERS -> SEP -> SFP -> SF1 -> EVENTS, spec §9). Per-dataset and
// per-file failures are captured into the summary; only context
// cancellation (or a nil-pool programming error) aborts the run, returning
// the partial summary alongside the error.
func (im *Importer) Run(ctx context.Context) (*Summary, error) {
	sum := &Summary{Started: time.Now(), Tables: make([]*TableStats, 0, len(im.datasets))}
	im.log.Info().
		Str("cache_dir", im.opts.CacheDir).
		Strs("datasets", im.datasets).
		Int("batch_size", im.opts.BatchSize).
		Time("since", im.opts.Since).
		Int("ticker_filter", len(im.tickerSet)).
		Msg("starting sharadar import")

	for _, ds := range im.datasets {
		stats := &TableStats{Dataset: ds}
		sum.Tables = append(sum.Tables, stats)

		var err error
		switch ds {
		case DatasetTickers:
			err = im.importTickers(ctx, stats)
		case DatasetSEP, DatasetSFP:
			err = im.importBars(ctx, ds, stats)
		case DatasetSF1:
			err = im.importPerTicker(ctx, DatasetSF1, stats)
		case DatasetEvents:
			err = im.importPerTicker(ctx, DatasetEvents, stats)
		}
		if err != nil {
			if isCtxErr(err) {
				sum.Finished = time.Now()
				return sum, fmt.Errorf("sharadar: import canceled during %s: %w", ds, err)
			}
			stats.recordError(err.Error())
			im.log.Warn().Err(err).Str("dataset", ds).Msg("dataset import failed; continuing")
			im.datasetsDone++
			im.emitProgress(stats, "", stats.RowsUpserted, true)
			continue
		}

		if err := im.recordSync(ctx, ds, stats.RowsUpserted); err != nil {
			if isCtxErr(err) {
				sum.Finished = time.Now()
				return sum, fmt.Errorf("sharadar: import canceled during %s sync record: %w", ds, err)
			}
			stats.recordError(fmt.Sprintf("dataset_sync: %v", err))
			im.log.Warn().Err(err).Str("dataset", ds).Msg("recording dataset_sync failed; continuing")
		}

		im.log.Info().
			Str("dataset", ds).
			Int("files", stats.FilesScanned).
			Int("files_failed", stats.FilesFailed).
			Int64("rows_read", stats.RowsRead).
			Int64("rows_skipped", stats.RowsSkipped).
			Int64("rows_failed", stats.RowsFailed).
			Int64("fields_nulled", stats.FieldsNulled).
			Int64("rows_upserted", stats.RowsUpserted).
			Msg("dataset import complete")

		im.datasetsDone++
		im.emitProgress(stats, "", stats.RowsUpserted, true)
	}

	sum.Finished = time.Now()
	return sum, nil
}

// recordSync upserts tms.dataset_sync (bootstrap row-count semantics, see
// sql.go).
func (im *Importer) recordSync(ctx context.Context, dataset string, rows int64) error {
	_, err := im.pool.Exec(ctx, datasetSyncSQL, dataset, rows)
	return err
}

// ---------------------------------------------------------------------------
// Staging loader
// ---------------------------------------------------------------------------

// loader owns one pooled connection with a session temp staging table and
// flushes buffered rows through CopyFrom + merge in bounded batches.
// Shared by the parquet importer and the API sync (sync.go).
type loader struct {
	conn      *pgxpool.Conn
	plan      stagingPlan
	batchSize int
	buf       [][]any
	seq       int64
	upserted  int64 // total rows affected (inserts + conflict updates)
	inserted  int64 // net-new keys only (Python writers' `added` semantics)
}

func newLoader(ctx context.Context, pool *pgxpool.Pool, plan stagingPlan, batchSize int) (*loader, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("sharadar: acquiring connection: %w", err)
	}
	if _, err := conn.Exec(ctx, plan.createSQL); err != nil {
		conn.Release()
		return nil, fmt.Errorf("sharadar: creating staging %s: %w", plan.staging, err)
	}
	// The pool may hand back a session that already ran an import: start clean.
	if _, err := conn.Exec(ctx, "TRUNCATE "+plan.staging); err != nil {
		conn.Release()
		return nil, fmt.Errorf("sharadar: truncating staging %s: %w", plan.staging, err)
	}
	return &loader{conn: conn, plan: plan, batchSize: batchSize, buf: make([][]any, 0, batchSize)}, nil
}

// add buffers one converted row (loader prepends the staging sequence) and
// flushes when the batch bound is reached.
func (l *loader) add(ctx context.Context, row []any) error {
	l.seq++
	staged := make([]any, 0, len(row)+1)
	staged = append(staged, l.seq)
	staged = append(staged, row...)
	l.buf = append(l.buf, staged)
	if len(l.buf) >= l.batchSize {
		return l.flush(ctx)
	}
	return nil
}

// flush copies the buffer into the staging table and merges it into the
// target inside one transaction.
func (l *loader) flush(ctx context.Context) error {
	if len(l.buf) == 0 {
		return nil
	}
	tx, err := l.conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("sharadar: beginning flush tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after Commit

	cols := append([]string{"seq"}, l.plan.columns...)
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{l.plan.staging}, cols, pgx.CopyFromRows(l.buf)); err != nil {
		return fmt.Errorf("sharadar: copy into %s: %w", l.plan.staging, err)
	}
	var total, netNew int64
	if err := tx.QueryRow(ctx, l.plan.mergeCountSQL).Scan(&total, &netNew); err != nil {
		return fmt.Errorf("sharadar: merging %s: %w", l.plan.staging, err)
	}
	if _, err := tx.Exec(ctx, "TRUNCATE "+l.plan.staging); err != nil {
		return fmt.Errorf("sharadar: truncating %s: %w", l.plan.staging, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("sharadar: committing flush: %w", err)
	}
	l.upserted += total
	l.inserted += netNew
	l.buf = l.buf[:0]
	return nil
}

// close drops the staging table and releases the connection.
func (l *loader) close() {
	// Best-effort cleanup with a short independent deadline so close works
	// even after cancellation.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = l.conn.Exec(ctx, "DROP TABLE IF EXISTS "+l.plan.staging)
	l.conn.Release()
}

// ---------------------------------------------------------------------------
// Dataset imports
// ---------------------------------------------------------------------------

func (im *Importer) wantTicker(t string) bool {
	if im.tickerSet == nil {
		return true
	}
	_, ok := im.tickerSet[t]
	return ok
}

func (im *Importer) progress(stats *TableStats, file string, upserted int64) {
	if stats.RowsRead%im.opts.ProgressEvery == 0 {
		im.log.Info().
			Str("dataset", stats.Dataset).
			Str("file", file).
			Int64("rows_read", stats.RowsRead).
			Int64("rows_upserted_so_far", upserted).
			Msg("import progress")
		im.emitProgress(stats, file, upserted, false)
	}
}

// emitProgress forwards one OnProgress callback when configured. The
// upserted argument overrides stats.RowsUpserted mid-dataset (the stat is
// only finalized at dataset end).
func (im *Importer) emitProgress(stats *TableStats, file string, upserted int64, done bool) {
	if im.opts.OnProgress == nil {
		return
	}
	im.opts.OnProgress(ProgressEvent{
		Dataset:       stats.Dataset,
		File:          file,
		RowsRead:      stats.RowsRead,
		RowsSkipped:   stats.RowsSkipped,
		RowsFailed:    stats.RowsFailed,
		RowsUpserted:  upserted,
		DatasetDone:   done,
		DatasetsDone:  im.datasetsDone,
		DatasetsTotal: len(im.datasets),
	})
}

// importBars loads SEP or SFP year partitions into tms.bars_daily.
func (im *Importer) importBars(ctx context.Context, dataset string, stats *TableStats) error {
	parts, err := yearPartitions(im.opts.CacheDir, dataset)
	if err != nil {
		return err
	}
	if len(parts) == 0 {
		im.log.Warn().Str("dataset", dataset).Msg("no year partitions found")
		return nil
	}

	ld, err := newLoader(ctx, im.pool, barsPlan(), im.opts.BatchSize)
	if err != nil {
		return err
	}
	defer ld.close()

	for _, p := range parts {
		if !im.opts.Since.IsZero() && p.Year < im.opts.Since.Year() {
			continue // whole partition predates --since
		}
		stats.FilesScanned++
		err := im.scanBarsFile(ctx, ld, p.Path, dataset, stats)
		if err != nil {
			if isCtxErr(err) {
				return err
			}
			stats.FilesFailed++
			stats.recordError(fmt.Sprintf("%s: %v", p.Path, err))
			im.log.Warn().Err(err).Str("file", p.Path).Msg("partition import failed; continuing")
		}
	}
	if err := ld.flush(ctx); err != nil {
		return err
	}
	stats.RowsUpserted = ld.upserted
	return nil
}

func (im *Importer) scanBarsFile(ctx context.Context, ld *loader, path, dataset string, stats *TableStats) error {
	return scanParquet(ctx, path, nil, func(rec arrow.Record) error {
		c := colsOf(rec.Schema())
		tickerCol, dateCol := c.idx("ticker"), c.idx("date")
		for i := 0; i < int(rec.NumRows()); i++ {
			stats.RowsRead++
			im.progress(stats, path, ld.upserted)

			if im.tickerSet != nil {
				t, ok := stringCell(rec, tickerCol, i)
				if !ok || !im.wantTicker(t) {
					stats.RowsSkipped++
					continue
				}
			}
			if !im.opts.Since.IsZero() {
				if d, ok := timeCell(rec, dateCol, i); ok && utcMidnight(d).Before(im.opts.Since) {
					stats.RowsSkipped++
					continue
				}
			}

			row, nulled, err := convertBarRow(rec, c, i, dataset)
			if err != nil {
				stats.RowsFailed++
				stats.recordError(err.Error())
				continue
			}
			stats.FieldsNulled += int64(nulled)
			if err := ld.add(ctx, row); err != nil {
				return err
			}
		}
		return nil
	})
}

// importPerTicker loads SF1 or EVENTS per-ticker files.
func (im *Importer) importPerTicker(ctx context.Context, dataset string, stats *TableStats) error {
	files, err := perTickerFiles(im.opts.CacheDir, dataset)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		im.log.Warn().Str("dataset", dataset).Msg("no per-ticker files found")
		return nil
	}

	var (
		plan      stagingPlan
		dateField string
		convert   func(rec arrow.Record, c colmap, row int) ([]any, int, error)
	)
	switch dataset {
	case DatasetSF1:
		plan, dateField, convert = sf1Plan(), "datekey", convertSF1Row
	case DatasetEvents:
		plan, dateField, convert = eventsPlan(), "date", convertEventRow
	default:
		return fmt.Errorf("sharadar: importPerTicker called with %q", dataset)
	}

	ld, err := newLoader(ctx, im.pool, plan, im.opts.BatchSize)
	if err != nil {
		return err
	}
	defer ld.close()

	for _, f := range files {
		if !im.wantTicker(strings.ToUpper(f.Ticker)) {
			continue
		}
		stats.FilesScanned++
		err := scanParquet(ctx, f.Path, nil, func(rec arrow.Record) error {
			c := colsOf(rec.Schema())
			dateCol := c.idx(dateField)
			for i := 0; i < int(rec.NumRows()); i++ {
				stats.RowsRead++
				im.progress(stats, f.Path, ld.upserted)

				if !im.opts.Since.IsZero() {
					if d, ok := timeCell(rec, dateCol, i); ok && utcMidnight(d).Before(im.opts.Since) {
						stats.RowsSkipped++
						continue
					}
				}
				row, nulled, err := convert(rec, c, i)
				if err != nil {
					stats.RowsFailed++
					stats.recordError(fmt.Sprintf("%s: %v", f.Path, err))
					continue
				}
				stats.FieldsNulled += int64(nulled)
				if err := ld.add(ctx, row); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			if isCtxErr(err) {
				return err
			}
			stats.FilesFailed++
			stats.recordError(fmt.Sprintf("%s: %v", f.Path, err))
			im.log.Warn().Err(err).Str("file", f.Path).Msg("per-ticker file import failed; continuing")
		}
	}
	if err := ld.flush(ctx); err != nil {
		return err
	}
	stats.RowsUpserted = ld.upserted
	return nil
}

// importTickers loads TICKERS.parquet into tms.tickers. With no --tickers
// filter it replicates the Python writer's full-overwrite (upsert + delete
// rows missing from the source) in one transaction. If the file is absent,
// it derives a degraded universe from the cache structure itself (see
// deriveTickerRows).
func (im *Importer) importTickers(ctx context.Context, stats *TableStats) error {
	ld, err := newLoader(ctx, im.pool, tickersPlan(), im.opts.BatchSize)
	if err != nil {
		return err
	}
	defer ld.close()
	// TICKERS merges once at the end (single transaction with the optional
	// delete-missing), so the incremental flush bound must not trigger.
	ld.batchSize = int(^uint(0) >> 1)

	path := tickersPath(im.opts.CacheDir)
	derived := false
	if _, statErr := os.Stat(path); statErr != nil {
		derived = true
		im.log.Warn().Str("path", path).
			Msg("TICKERS.parquet missing — deriving a degraded universe (ticker + table only) from SF1 file listing and SFP partitions")
		rows, derr := im.deriveTickerRows(ctx, stats)
		if derr != nil {
			return derr
		}
		for _, row := range rows {
			if err := ld.add(ctx, row); err != nil {
				return err
			}
		}
	} else {
		stats.FilesScanned++
		err := scanParquet(ctx, path, nil, func(rec arrow.Record) error {
			c := colsOf(rec.Schema())
			tickerCol := c.idx("ticker")
			for i := 0; i < int(rec.NumRows()); i++ {
				stats.RowsRead++
				im.progress(stats, path, ld.upserted)

				if im.tickerSet != nil {
					t, ok := stringCell(rec, tickerCol, i)
					if !ok || !im.wantTicker(t) {
						stats.RowsSkipped++
						continue
					}
				}
				row, nulled, err := convertTickerRow(rec, c, i)
				if err != nil {
					stats.RowsFailed++
					stats.recordError(err.Error())
					continue
				}
				stats.FieldsNulled += int64(nulled)
				if err := ld.add(ctx, row); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}

	// Single transaction: copy + merge (+ full-overwrite delete when the
	// entire real universe file was staged).
	tx, err := ld.conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("sharadar: beginning tickers tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if len(ld.buf) > 0 {
		cols := append([]string{"seq"}, ld.plan.columns...)
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{ld.plan.staging}, cols, pgx.CopyFromRows(ld.buf)); err != nil {
			return fmt.Errorf("sharadar: copy into %s: %w", ld.plan.staging, err)
		}
	}
	ct, err := tx.Exec(ctx, ld.plan.upsertSQL)
	if err != nil {
		return fmt.Errorf("sharadar: merging tickers: %w", err)
	}
	if im.tickerSet == nil && !derived {
		if _, err := tx.Exec(ctx, tickersDeleteMissingSQL); err != nil {
			return fmt.Errorf("sharadar: deleting stale tickers: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, "TRUNCATE "+ld.plan.staging); err != nil {
		return fmt.Errorf("sharadar: truncating %s: %w", ld.plan.staging, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("sharadar: committing tickers: %w", err)
	}
	ld.buf = ld.buf[:0]
	stats.RowsUpserted = ct.RowsAffected()
	return nil
}

// deriveTickerRows builds a minimal universe when TICKERS.parquet is absent:
// SF1 common stocks from the SF1/ticker=*.parquet listing, SFP funds from
// the distinct ticker column of the SFP year partitions. Only ticker and
// table_name are populated (name/sector/dates unknown); is_delisted defaults
// to false. This is a degraded fallback so SEP/SF1 joins keep working — it
// never deletes existing richer rows.
func (im *Importer) deriveTickerRows(ctx context.Context, stats *TableStats) ([][]any, error) {
	newRow := func(ticker, table string) []any {
		return []any{ticker, nil, nil, false, nil, nil, nil, table, nil, nil, nil}
	}

	seen := make(map[string]struct{})
	var rows [][]any

	sf1Files, err := perTickerFiles(im.opts.CacheDir, DatasetSF1)
	if err != nil {
		return nil, err
	}
	for _, f := range sf1Files {
		t := strings.ToUpper(f.Ticker)
		if !im.wantTicker(t) {
			continue
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		rows = append(rows, newRow(f.Ticker, "SF1"))
		stats.RowsRead++
	}

	parts, err := yearPartitions(im.opts.CacheDir, DatasetSFP)
	if err != nil {
		return nil, err
	}
	sfpSeen := make(map[string]struct{})
	for _, p := range parts {
		stats.FilesScanned++
		err := scanParquet(ctx, p.Path, []string{"ticker"}, func(rec arrow.Record) error {
			c := colsOf(rec.Schema())
			tickerCol := c.idx("ticker")
			for i := 0; i < int(rec.NumRows()); i++ {
				t, ok := stringCell(rec, tickerCol, i)
				if !ok || t == "" {
					continue
				}
				key := strings.ToUpper(t)
				if !im.wantTicker(key) {
					continue
				}
				if _, dup := sfpSeen[key]; dup {
					continue
				}
				sfpSeen[key] = struct{}{}
				if _, dup := seen[key]; dup {
					continue // SF1 listing wins on the (pathological) overlap
				}
				rows = append(rows, newRow(t, "SFP"))
				stats.RowsRead++
			}
			return nil
		})
		if err != nil {
			if isCtxErr(err) {
				return nil, err
			}
			stats.FilesFailed++
			stats.recordError(fmt.Sprintf("derive tickers from %s: %v", p.Path, err))
		}
	}

	sort.Slice(rows, func(i, j int) bool { return rows[i][0].(string) < rows[j][0].(string) })
	return rows, nil
}
