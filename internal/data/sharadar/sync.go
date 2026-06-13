package sharadar

// sync.go is the API -> PostgreSQL incremental sync, the relational
// counterpart of the Python parquet pipeline:
//
//   - Syncer.EnsureFresh mirrors ensure_cache_fresh (catchup.py; spec §8.2
//     [MUST-MATCH] flow): SEP watermark gate ("not-bootstrapped" — never
//     auto-bootstraps), per-trading-day SEP-then-SFP updates through T-1,
//     then one TICKERS full overwrite, then SF1 and EVENTS refresh driven
//     by the stored ticker list; every step is warn-and-continue, the
//     watermark (tms.dataset_sync, the .meta.json counterpart) is persisted
//     after every day/step so a crash preserves progress.
//   - Syncer.Bootstrap mirrors the sync-universe bootstrap CLI (spec §9):
//     TICKERS -> SEP -> SFP -> SF1 -> EVENTS, with SEP/SFP pulled in
//     calendar-quarter chunks (spec §6.1) and SF1/EVENTS in 500-ticker
//     batches; API errors abort (CLI parity), and the watermark is saved
//     after each dataset (sanctioned [IMPROVE] over the original's single
//     end-of-run save).
//
// Merge semantics: rows stream from the client through the shared staging
// loader (importer.go) into INSERT ... ON CONFLICT merges — "new rows win"
// per the spec §6 dedup keys (SEP/SFP: ticker+date(+source column); SF1:
// ticker+datekey+dimension; EVENTS: ticker+date+eventcodes; TICKERS: full
// overwrite). Net-new counting matches the writers' `added` (revisions
// applied but not counted). Because rows stream in bounded batches, an API
// failure mid-dataset can leave earlier batches committed; the merge is
// idempotent, so the next run converges (the Python fetch-then-write had
// no partial state but also no bounded memory).
//
// Watermark/today deviations (P1 locked decisions, documented in
// docs/spec/data-sharadar.md addendum): all "today"/date logic is the
// America/New_York trading date via internal/data/calendar, replacing the
// original's UTC/local mix; NYSE holidays are skipped instead of issuing
// zero-row weekday calls.
//
// Additionally every run writes an audit row per dataset into
// tms.dataset_sync_runs (started/finished/rows/status/error, migration
// 000009) — additive observability the original lacked.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
)

// Run kinds recorded in tms.dataset_sync_runs.
const (
	runKindBootstrap = "bootstrap"
	runKindCatchup   = "catchup"
)

// CatchupReport mirrors the Python CatchupReport (spec §8.1).
type CatchupReport struct {
	// SkippedReason is non-empty when the whole catchup was skipped
	// ("not-bootstrapped").
	SkippedReason string
	// DaysAttempted / DaysSucceeded count the per-day SEP+SFP loop; a day
	// succeeds only when both datasets succeeded.
	DaysAttempted int
	DaysSucceeded int
	// RowsAdded has all five dataset keys (zero-initialized) once the
	// catchup ran; nil when skipped or already fresh.
	RowsAdded map[string]int64
	// Errors collects warn-and-continue failures ("SEP 2026-06-10: ...").
	Errors []string
}

// DidWork mirrors the Python property: true iff the day loop ran or the
// dataset refreshes were reached.
func (r *CatchupReport) DidWork() bool {
	return r.DaysAttempted > 0 || r.RowsAdded != nil
}

// BootstrapOptions configures Syncer.Bootstrap.
type BootstrapOptions struct {
	// Start / End bound the SEP/SFP backfill, inclusive.
	Start calendar.Date
	End   calendar.Date
	// Tickers, when non-empty, narrows SEP/SFP/SF1/EVENTS to these symbols
	// (the smoke-test path of the Python CLI). TICKERS is always synced in
	// full — the row filter is the survivorship policy, not the selection.
	Tickers []string
}

// ---------------------------------------------------------------------------
// Store seam — the database side of the sync, faked in unit tests
// ---------------------------------------------------------------------------

// rowSink receives converted rows for one dataset merge.
type rowSink interface {
	// Add buffers one row (staging column order of the dataset's plan).
	Add(ctx context.Context, row []any) error
	// Close flushes outstanding rows, returns the net-new row count and
	// releases resources. Call exactly once on success paths.
	Close(ctx context.Context) (added int64, err error)
	// Abort releases resources without a final flush (already-flushed
	// batches remain merged; idempotent re-runs converge).
	Abort()
}

// syncStore is the persistence seam of the Syncer.
type syncStore interface {
	// Watermark reads tms.dataset_sync for dataset; synced=false when the
	// dataset has never been synced (no row or NULL last_sync).
	Watermark(ctx context.Context, dataset string) (lastSync time.Time, rowCount int64, synced bool, err error)
	// RecordSync upserts the watermark: last_sync = now, row_count as given
	// (CacheMeta.record_sync parity, spec §5).
	RecordSync(ctx context.Context, dataset string, rowCount int64) error
	// NewMerge opens a merge session for SEP/SFP/SF1/EVENTS.
	NewMerge(ctx context.Context, dataset string) (rowSink, error)
	// OverwriteTickers replaces tms.tickers with rows (upsert + delete
	// missing, the writer's full-overwrite semantics, spec §2.5).
	OverwriteTickers(ctx context.Context, rows [][]any) (int64, error)
	// ListTickers returns all stored tickers sorted ascending (the
	// post-TICKERS reload that drives SF1/EVENTS, spec §8.2 step 7).
	ListTickers(ctx context.Context) ([]string, error)
	// StartRun / FinishRun maintain tms.dataset_sync_runs audit rows.
	StartRun(ctx context.Context, dataset, kind string) (int64, error)
	FinishRun(ctx context.Context, runID int64, rows int64, runErr string) error
}

// ---------------------------------------------------------------------------
// Syncer
// ---------------------------------------------------------------------------

// Syncer drives the Nasdaq Data Link -> PostgreSQL sync.
type Syncer struct {
	client TableFetcher
	store  syncStore
	cal    *calendar.Calendar
	log    zerolog.Logger
	now    func() time.Time

	// fullRefetch disables the lastupdated.gte incremental filter on
	// SF1/EVENTS catchup (spec §6.6 [IMPROVE]), reproducing the original
	// full-history refetch byte-for-byte.
	fullRefetch bool
}

// SyncerOption customizes a Syncer.
type SyncerOption func(*Syncer)

// WithFullRefetch forces full-history SF1/EVENTS pulls on catchup
// (original Python behavior; default is the incremental lastupdated
// filter sanctioned by spec §6.6).
func WithFullRefetch() SyncerOption { return func(s *Syncer) { s.fullRefetch = true } }

// WithSyncLogger attaches a structured logger.
func WithSyncLogger(l zerolog.Logger) SyncerOption { return func(s *Syncer) { s.log = l } }

// withClock injects a deterministic clock (tests).
func withClock(now func() time.Time) SyncerOption { return func(s *Syncer) { s.now = now } }

// withStore injects a fake store (tests).
func withStore(st syncStore) SyncerOption { return func(s *Syncer) { s.store = st } }

// NewSyncer builds a Syncer over a live pool. cal supplies the
// America/New_York trading-date normalization (P1 locked decision 2).
func NewSyncer(pool *pgxpool.Pool, client TableFetcher, cal *calendar.Calendar, opts ...SyncerOption) (*Syncer, error) {
	if client == nil {
		return nil, errors.New("sharadar: nil table client")
	}
	if cal == nil {
		return nil, errors.New("sharadar: nil calendar")
	}
	s := &Syncer{
		client: client,
		cal:    cal,
		log:    zerolog.Nop(),
		now:    time.Now,
	}
	for _, o := range opts {
		o(s)
	}
	if s.store == nil {
		if pool == nil {
			return nil, errors.New("sharadar: nil connection pool")
		}
		s.store = &pgStore{pool: pool, batchSize: DefaultBatchSize}
	}
	s.log = s.log.With().Str("component", "sharadar-sync").Logger()
	return s, nil
}

// ---------------------------------------------------------------------------
// EnsureFresh — ensure_cache_fresh parity (spec §8.2)
// ---------------------------------------------------------------------------

// EnsureFresh catches the relational store up to T-1. Per-step failures
// are collected into the report (warn-and-continue, spec §8.2); the
// returned error is non-nil only for context cancellation or store-level
// failures that make continuing meaningless (the partial report is
// returned alongside).
func (s *Syncer) EnsureFresh(ctx context.Context) (*CatchupReport, error) {
	report := &CatchupReport{}

	lastSEP, _, synced, err := s.store.Watermark(ctx, DatasetSEP)
	if err != nil {
		return report, fmt.Errorf("sharadar: reading SEP watermark: %w", err)
	}
	if !synced {
		s.log.Warn().Msg("dataset_sync has no SEP last_sync — never bootstrapped; " +
			"skipping auto-catchup; run `tms sync bootstrap --start YYYY-MM-DD --end YYYY-MM-DD` once")
		report.SkippedReason = "not-bootstrapped"
		return report, nil
	}

	today := calendar.DateOf(s.now(), s.cal.Location())
	start, target := catchupWindow(lastSEP, today, s.cal.Location())
	days := tradingDays(s.cal, start, target)
	if len(days) == 0 {
		s.log.Info().Stringer("target", target).Time("last_sync", lastSEP).
			Msg("store fresh — no catchup needed")
		return report, nil
	}

	s.log.Info().Int("days", len(days)).Stringer("from", days[0]).Stringer("through", days[len(days)-1]).
		Msg("starting auto-catchup")

	report.DaysAttempted = len(days)
	report.RowsAdded = map[string]int64{
		DatasetSEP: 0, DatasetSFP: 0, DatasetSF1: 0, DatasetEvents: 0, DatasetTickers: 0,
	}

	// Baseline cumulative row counts (CacheMeta.row_counts parity: update
	// semantics are previous + net-new, spec §5).
	counts := make(map[string]int64, len(DatasetOrder))
	for _, ds := range DatasetOrder {
		_, n, _, err := s.store.Watermark(ctx, ds)
		if err != nil {
			return report, fmt.Errorf("sharadar: reading %s watermark: %w", ds, err)
		}
		counts[ds] = n
	}

	sepRun := s.startRun(ctx, DatasetSEP, runKindCatchup)
	sfpRun := s.startRun(ctx, DatasetSFP, runKindCatchup)
	var sepErrs, sfpErrs []string

	// Per-day SEP then SFP — SEP first so quota exhaustion mid-loop leaves
	// the most-used dataset most complete (spec §8.2 step 5).
	for _, d := range days {
		dayOK := true
		for _, ds := range []string{DatasetSEP, DatasetSFP} {
			n, err := s.syncBarsDay(ctx, ds, d)
			if err != nil {
				if isCtxErr(err) {
					s.finishRun(ctx, sepRun, report.RowsAdded[DatasetSEP], strings.Join(append(sepErrs, "canceled"), "; "))
					s.finishRun(ctx, sfpRun, report.RowsAdded[DatasetSFP], strings.Join(append(sfpErrs, "canceled"), "; "))
					return report, fmt.Errorf("sharadar: catchup canceled during %s %s: %w", ds, d, err)
				}
				dayOK = false
				msg := fmt.Sprintf("%s %s: %v", ds, d, err)
				report.Errors = append(report.Errors, msg)
				if ds == DatasetSEP {
					sepErrs = append(sepErrs, msg)
				} else {
					sfpErrs = append(sfpErrs, msg)
				}
				s.log.Warn().Err(err).Str("dataset", ds).Stringer("asof", d).Msg("auto-catchup day failed; continuing")
				continue
			}
			report.RowsAdded[ds] += n
			counts[ds] += n
			// Persist the watermark after each dataset/day so a crash
			// preserves progress (the per-day meta.save of the original).
			if err := s.store.RecordSync(ctx, ds, counts[ds]); err != nil {
				if isCtxErr(err) {
					return report, fmt.Errorf("sharadar: catchup canceled recording %s watermark: %w", ds, err)
				}
				report.Errors = append(report.Errors, fmt.Sprintf("%s %s: recording watermark: %v", ds, d, err))
				s.log.Warn().Err(err).Str("dataset", ds).Msg("recording watermark failed; continuing")
			}
		}
		if dayOK {
			report.DaysSucceeded++
		}
	}
	s.finishRun(ctx, sepRun, report.RowsAdded[DatasetSEP], strings.Join(sepErrs, "; "))
	s.finishRun(ctx, sfpRun, report.RowsAdded[DatasetSFP], strings.Join(sfpErrs, "; "))

	// TICKERS refresh — once, after the day loop, so SF1/EVENTS see newly
	// listed/delisted tickers (spec §8.2 step 6).
	tickersRun := s.startRun(ctx, DatasetTickers, runKindCatchup)
	n, err := s.syncTickers(ctx)
	if err != nil {
		if isCtxErr(err) {
			s.finishRun(ctx, tickersRun, 0, "canceled")
			return report, fmt.Errorf("sharadar: catchup canceled during TICKERS: %w", err)
		}
		report.Errors = append(report.Errors, fmt.Sprintf("TICKERS: %v", err))
		s.log.Warn().Err(err).Msg("auto-catchup TICKERS failed; continuing")
		s.finishRun(ctx, tickersRun, 0, fmt.Sprintf("TICKERS: %v", err))
	} else {
		report.RowsAdded[DatasetTickers] = n
		// TICKERS row_count is always the absolute rewrite count (spec §5).
		if err := s.store.RecordSync(ctx, DatasetTickers, n); err != nil {
			if isCtxErr(err) {
				return report, fmt.Errorf("sharadar: catchup canceled recording TICKERS watermark: %w", err)
			}
			report.Errors = append(report.Errors, fmt.Sprintf("TICKERS: recording watermark: %v", err))
		}
		s.finishRun(ctx, tickersRun, n, "")
	}

	// Reload the ticker list from the store; a TICKERS failure above does
	// not prevent SF1/EVENTS (they use the previous stored list, spec §8.2
	// step 8).
	tickers, err := s.store.ListTickers(ctx)
	if err != nil {
		if isCtxErr(err) {
			return report, fmt.Errorf("sharadar: catchup canceled listing tickers: %w", err)
		}
		report.Errors = append(report.Errors, fmt.Sprintf("TICKERS list: %v", err))
		tickers = nil
	}
	if len(tickers) == 0 {
		report.Errors = append(report.Errors, "TICKERS list empty — skipping SF1 / EVENTS")
		s.log.Warn().Msg("auto-catchup: TICKERS empty; skipping SF1 + EVENTS refresh")
	} else {
		for _, ds := range []string{DatasetSF1, DatasetEvents} {
			run := s.startRun(ctx, ds, runKindCatchup)
			n, err := s.syncPerTicker(ctx, ds, tickers, !s.fullRefetch)
			if err != nil {
				if isCtxErr(err) {
					s.finishRun(ctx, run, 0, "canceled")
					return report, fmt.Errorf("sharadar: catchup canceled during %s: %w", ds, err)
				}
				report.Errors = append(report.Errors, fmt.Sprintf("%s: %v", ds, err))
				s.log.Warn().Err(err).Str("dataset", ds).Msg("auto-catchup refresh failed; continuing")
				s.finishRun(ctx, run, 0, fmt.Sprintf("%s: %v", ds, err))
				continue
			}
			report.RowsAdded[ds] = n // absolute per spec §8.2 step 8
			if err := s.store.RecordSync(ctx, ds, counts[ds]+n); err != nil {
				if isCtxErr(err) {
					return report, fmt.Errorf("sharadar: catchup canceled recording %s watermark: %w", ds, err)
				}
				report.Errors = append(report.Errors, fmt.Sprintf("%s: recording watermark: %v", ds, err))
			}
			s.finishRun(ctx, run, n, "")
		}
	}

	s.log.Info().
		Int("days_ok", report.DaysSucceeded).Int("days", report.DaysAttempted).
		Interface("rows_added", report.RowsAdded).Int("errors", len(report.Errors)).
		Msg("auto-catchup done")
	return report, nil
}

// ---------------------------------------------------------------------------
// Bootstrap — sync-universe bootstrap parity (spec §9)
// ---------------------------------------------------------------------------

// Bootstrap backfills all five datasets. Unlike EnsureFresh it propagates
// the first error (CLI parity: a failed bootstrap step aborts), returning
// the partial per-dataset row counts gathered so far.
func (s *Syncer) Bootstrap(ctx context.Context, opts BootstrapOptions) (map[string]int64, error) {
	if opts.End.Before(opts.Start) {
		return nil, fmt.Errorf("sharadar: bootstrap end %s before start %s", opts.End, opts.Start)
	}
	rows := make(map[string]int64, len(DatasetOrder))

	// 1. TICKERS (always full universe).
	if err := s.bootstrapStep(ctx, DatasetTickers, rows, func() (int64, error) {
		return s.syncTickers(ctx)
	}); err != nil {
		return rows, err
	}

	tickers := opts.Tickers
	if len(tickers) == 0 {
		var err error
		tickers, err = s.store.ListTickers(ctx)
		if err != nil {
			return rows, fmt.Errorf("sharadar: bootstrap listing tickers: %w", err)
		}
	}
	if len(tickers) == 0 {
		return rows, errors.New("sharadar: bootstrap aborted: ticker list empty after TICKERS sync")
	}

	// 2-3. SEP, SFP in calendar-quarter chunks (spec §6.1/§6.2).
	for _, ds := range []string{DatasetSEP, DatasetSFP} {
		if err := s.bootstrapStep(ctx, ds, rows, func() (int64, error) {
			return s.bootstrapBars(ctx, ds, opts)
		}); err != nil {
			return rows, err
		}
	}

	// 4-5. SF1, EVENTS full history in 500-ticker batches (spec §6.4/§6.5).
	for _, ds := range []string{DatasetSF1, DatasetEvents} {
		ds := ds
		if err := s.bootstrapStep(ctx, ds, rows, func() (int64, error) {
			return s.syncPerTicker(ctx, ds, tickers, false)
		}); err != nil {
			return rows, err
		}
	}
	s.log.Info().Interface("rows", rows).Msg("bootstrap complete")
	return rows, nil
}

// bootstrapStep wraps one dataset step with run audit + watermark save
// (bootstrap row_count = rows written this run, spec §5).
func (s *Syncer) bootstrapStep(ctx context.Context, dataset string, rows map[string]int64, step func() (int64, error)) error {
	run := s.startRun(ctx, dataset, runKindBootstrap)
	n, err := step()
	if err != nil {
		s.finishRun(ctx, run, n, err.Error())
		return fmt.Errorf("sharadar: bootstrap %s: %w", dataset, err)
	}
	rows[dataset] = n
	s.finishRun(ctx, run, n, "")
	if err := s.store.RecordSync(ctx, dataset, n); err != nil {
		return fmt.Errorf("sharadar: bootstrap recording %s watermark: %w", dataset, err)
	}
	s.log.Info().Str("dataset", dataset).Int64("rows", n).Msg("bootstrap step complete")
	return nil
}

// bootstrapBars pulls one bar dataset over quarter chunks.
func (s *Syncer) bootstrapBars(ctx context.Context, dataset string, opts BootstrapOptions) (int64, error) {
	chunks, err := dateChunks(opts.Start, opts.End, 3)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, ch := range chunks {
		filters := DateRangeFilters(ch.Start.String(), ch.End.String())
		if len(opts.Tickers) > 0 {
			filters = append(filters, TickersFilter(opts.Tickers))
		}
		n, err := s.mergeBars(ctx, dataset, filters)
		if err != nil {
			return total, fmt.Errorf("chunk [%s, %s]: %w", ch.Start, ch.End, err)
		}
		if n == 0 {
			s.log.Info().Str("dataset", dataset).Stringer("from", ch.Start).Stringer("to", ch.End).
				Msg("bootstrap chunk returned 0 rows")
		}
		total += n
	}
	return total, nil
}

// ---------------------------------------------------------------------------
// Dataset sync primitives
// ---------------------------------------------------------------------------

// syncBarsDay is update_sep/update_sfp parity: single-day pull, merge,
// net-new count; an empty day returns 0 without touching the table.
func (s *Syncer) syncBarsDay(ctx context.Context, dataset string, asof calendar.Date) (int64, error) {
	return s.mergeBars(ctx, dataset, DateRangeFilters(asof.String(), asof.String()))
}

// mergeBars streams one SEP/SFP API call into the bars merge.
func (s *Syncer) mergeBars(ctx context.Context, dataset string, filters []Filter) (int64, error) {
	sink, err := s.store.NewMerge(ctx, dataset)
	if err != nil {
		return 0, err
	}
	closed := false
	defer func() {
		if !closed {
			sink.Abort()
		}
	}()

	var streamed, badRows int64
	_, err = s.client.GetTable(ctx, "SHARADAR/"+dataset, filters, func(r Row) error {
		row, _, cerr := convertBarAPIRow(r, dataset)
		if cerr != nil {
			badRows++
			return nil // skip-and-count, importer parity
		}
		streamed++
		return sink.Add(ctx, row)
	})
	if err != nil {
		return 0, err
	}
	if badRows > 0 {
		s.log.Warn().Str("dataset", dataset).Int64("rows", badRows).Msg("unconvertible API rows skipped")
	}
	if streamed == 0 {
		return 0, nil // empty day: nothing touched (spec §6.3)
	}
	added, err := sink.Close(ctx)
	closed = true
	return added, err
}

// syncPerTicker is update_sf1/update_events parity: 500-ticker batches,
// full history per batch; with incremental=true the lastupdated.gte
// watermark filter (spec §6.6 [IMPROVE]) narrows the pull, falling back to
// full history when the dataset has never been synced.
func (s *Syncer) syncPerTicker(ctx context.Context, dataset string, tickers []string, incremental bool) (int64, error) {
	if len(tickers) == 0 {
		return 0, nil // no API calls (spec §6.4)
	}

	var baseFilters []Filter
	if incremental {
		last, _, synced, err := s.store.Watermark(ctx, dataset)
		if err != nil {
			return 0, fmt.Errorf("reading %s watermark: %w", dataset, err)
		}
		if synced {
			// Overlap by one day (NY date of the previous sync) — Sharadar
			// stamps lastupdated by date, and the idempotent merge makes
			// the repull safe.
			since := calendar.DateOf(last, s.cal.Location())
			baseFilters = append(baseFilters, LastUpdatedGTEFilter(since.String()))
		}
	}

	convert := convertSF1APIRow
	if dataset == DatasetEvents {
		convert = convertEventAPIRow
	}

	sink, err := s.store.NewMerge(ctx, dataset)
	if err != nil {
		return 0, err
	}
	closed := false
	defer func() {
		if !closed {
			sink.Abort()
		}
	}()

	var badRows int64
	for _, batch := range batchTickers(tickers, sf1TickerBatchSize) {
		filters := append(append([]Filter(nil), baseFilters...), TickersFilter(batch))
		_, err := s.client.GetTable(ctx, "SHARADAR/"+dataset, filters, func(r Row) error {
			row, _, cerr := convert(r)
			if cerr != nil {
				badRows++
				return nil
			}
			return sink.Add(ctx, row)
		})
		if err != nil {
			return 0, err
		}
	}
	if badRows > 0 {
		s.log.Warn().Str("dataset", dataset).Int64("rows", badRows).Msg("unconvertible API rows skipped")
	}
	added, err := sink.Close(ctx)
	closed = true
	return added, err
}

// syncTickers is write_tickers parity (spec §2.5): full unfiltered API
// pull, survivorship row filter, sort by ticker ascending, full overwrite.
// Returns the number of rows written (the full universe count).
func (s *Syncer) syncTickers(ctx context.Context) (int64, error) {
	var (
		rows                         [][]any
		dropped, badRows             int64
		sf1Active, sf1Delisted, nSFP int64
	)
	_, err := s.client.GetTable(ctx, "SHARADAR/TICKERS", nil, func(r Row) error {
		table, _ := r.Str("table")
		category, _ := r.Str("category") // missing/NaN -> "" (fillna parity)
		isDelisted, _ := r.Str("isdelisted")
		if !keepTickerRow(table, category, isDelisted) {
			dropped++
			return nil
		}
		row, _, cerr := convertTickerAPIRow(r)
		if cerr != nil {
			badRows++
			return nil
		}
		switch {
		case table == "SF1" && isDelisted == "N":
			sf1Active++
		case table == "SF1":
			sf1Delisted++
		default:
			nSFP++
		}
		rows = append(rows, row)
		return nil
	})
	if err != nil {
		return 0, err
	}
	if badRows > 0 {
		s.log.Warn().Int64("rows", badRows).Msg("unconvertible TICKERS API rows skipped")
	}
	// Sorted by ticker ascending, the writer's output order (spec §2.5).
	sort.SliceStable(rows, func(i, j int) bool { return rows[i][0].(string) < rows[j][0].(string) })

	n, err := s.store.OverwriteTickers(ctx, rows)
	if err != nil {
		return 0, err
	}
	s.log.Info().Int64("rows", n).Int64("sf1_active", sf1Active).Int64("sf1_delisted", sf1Delisted).
		Int64("sfp", nSFP).Int64("dropped", dropped).Msg("tickers universe overwritten")
	return n, nil
}

// startRun / finishRun keep the audit trail; bookkeeping failures are
// logged but never affect the sync outcome.
func (s *Syncer) startRun(ctx context.Context, dataset, kind string) int64 {
	id, err := s.store.StartRun(ctx, dataset, kind)
	if err != nil {
		s.log.Warn().Err(err).Str("dataset", dataset).Msg("recording sync run start failed")
		return 0
	}
	return id
}

func (s *Syncer) finishRun(ctx context.Context, runID, rows int64, runErr string) {
	if runID == 0 {
		return
	}
	if err := s.store.FinishRun(ctx, runID, rows, runErr); err != nil {
		s.log.Warn().Err(err).Int64("run_id", runID).Msg("recording sync run finish failed")
	}
}

// ---------------------------------------------------------------------------
// pgStore — the live syncStore over pgxpool
// ---------------------------------------------------------------------------

const watermarkSQL = "SELECT last_sync, row_count FROM tms.dataset_sync WHERE dataset = $1"

const startRunSQL = "INSERT INTO tms.dataset_sync_runs (dataset, kind) VALUES ($1, $2) RETURNING id"

const finishRunSQL = `
UPDATE tms.dataset_sync_runs
SET finished_at = now(),
    rows_added  = GREATEST($2, 0),
    status      = CASE WHEN $3 = '' THEN 'ok' ELSE 'error' END,
    error       = NULLIF($3, '')
WHERE id = $1`

const listTickersSQL = "SELECT ticker FROM tms.tickers ORDER BY ticker"

type pgStore struct {
	pool      *pgxpool.Pool
	batchSize int
}

func (st *pgStore) Watermark(ctx context.Context, dataset string) (time.Time, int64, bool, error) {
	var (
		last  *time.Time
		count int64
	)
	err := st.pool.QueryRow(ctx, watermarkSQL, dataset).Scan(&last, &count)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, 0, false, nil
	}
	if err != nil {
		return time.Time{}, 0, false, fmt.Errorf("sharadar: reading dataset_sync %s: %w", dataset, err)
	}
	if last == nil {
		return time.Time{}, count, false, nil
	}
	return *last, count, true, nil
}

func (st *pgStore) RecordSync(ctx context.Context, dataset string, rowCount int64) error {
	_, err := st.pool.Exec(ctx, datasetSyncSQL, dataset, rowCount)
	return err
}

func (st *pgStore) NewMerge(ctx context.Context, dataset string) (rowSink, error) {
	var plan stagingPlan
	switch dataset {
	case DatasetSEP, DatasetSFP:
		plan = barsPlan()
	case DatasetSF1:
		plan = sf1Plan()
	case DatasetEvents:
		plan = eventsPlan()
	default:
		return nil, fmt.Errorf("sharadar: no merge plan for dataset %q", dataset)
	}
	ld, err := newLoader(ctx, st.pool, plan, st.batchSize)
	if err != nil {
		return nil, err
	}
	return &pgSink{ld: ld}, nil
}

// pgSink adapts the shared staging loader to the rowSink seam.
type pgSink struct {
	ld *loader
}

func (s *pgSink) Add(ctx context.Context, row []any) error { return s.ld.add(ctx, row) }

func (s *pgSink) Close(ctx context.Context) (int64, error) {
	defer s.ld.close()
	if err := s.ld.flush(ctx); err != nil {
		return s.ld.inserted, err
	}
	return s.ld.inserted, nil
}

func (s *pgSink) Abort() { s.ld.close() }

// OverwriteTickers stages all rows and applies upsert + delete-missing in
// one transaction (the Python writer's full-overwrite, spec §2.5; same
// shape as the importer's TICKERS path).
func (st *pgStore) OverwriteTickers(ctx context.Context, rows [][]any) (int64, error) {
	ld, err := newLoader(ctx, st.pool, tickersPlan(), len(rows)+1)
	if err != nil {
		return 0, err
	}
	defer ld.close()
	for _, row := range rows {
		if err := ld.add(ctx, row); err != nil {
			return 0, err
		}
	}

	tx, err := ld.conn.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("sharadar: beginning tickers tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after Commit

	if len(ld.buf) > 0 {
		cols := append([]string{"seq"}, ld.plan.columns...)
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{ld.plan.staging}, cols, pgx.CopyFromRows(ld.buf)); err != nil {
			return 0, fmt.Errorf("sharadar: copy into %s: %w", ld.plan.staging, err)
		}
	}
	if _, err := tx.Exec(ctx, ld.plan.upsertSQL); err != nil {
		return 0, fmt.Errorf("sharadar: merging tickers: %w", err)
	}
	if _, err := tx.Exec(ctx, tickersDeleteMissingSQL); err != nil {
		return 0, fmt.Errorf("sharadar: deleting stale tickers: %w", err)
	}
	if _, err := tx.Exec(ctx, "TRUNCATE "+ld.plan.staging); err != nil {
		return 0, fmt.Errorf("sharadar: truncating %s: %w", ld.plan.staging, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("sharadar: committing tickers: %w", err)
	}
	ld.buf = ld.buf[:0]
	// write_tickers returns the number of rows written (spec §2.5).
	return int64(len(rows)), nil
}

func (st *pgStore) ListTickers(ctx context.Context) ([]string, error) {
	rows, err := st.pool.Query(ctx, listTickersSQL)
	if err != nil {
		return nil, fmt.Errorf("sharadar: listing tickers: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, fmt.Errorf("sharadar: scanning ticker: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sharadar: listing tickers: %w", err)
	}
	return out, nil
}

func (st *pgStore) StartRun(ctx context.Context, dataset, kind string) (int64, error) {
	var id int64
	if err := st.pool.QueryRow(ctx, startRunSQL, dataset, kind).Scan(&id); err != nil {
		return 0, fmt.Errorf("sharadar: starting sync run %s/%s: %w", dataset, kind, err)
	}
	return id, nil
}

func (st *pgStore) FinishRun(ctx context.Context, runID, rows int64, runErr string) error {
	_, err := st.pool.Exec(ctx, finishRunSQL, runID, rows, runErr)
	if err != nil {
		return fmt.Errorf("sharadar: finishing sync run %d: %w", runID, err)
	}
	return nil
}
