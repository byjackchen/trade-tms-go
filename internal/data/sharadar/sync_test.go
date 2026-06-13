package sharadar

// sync_test.go drives the Syncer against a fake client + in-memory store,
// pinning the ensure_cache_fresh flow to the Python catchup oracle
// (test_catchup.py: per-day SEP/SFP interleaving, single TICKERS/SF1/EVENTS
// refresh, warn-and-continue, watermark accumulation) and the bootstrap
// flow to the sync-universe CLI order with quarter-chunked SEP/SFP calls.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// fakeResponse is the canned result for one dataset.
type fakeResponse struct {
	cols []string
	rows [][]any
	err  error
}

type fakeCall struct {
	dataset string
	filters []Filter
}

// fakeClient implements TableFetcher with per-dataset canned rows.
type fakeClient struct {
	responses map[string]fakeResponse // key: dataset (e.g. "SHARADAR/SEP")
	calls     []fakeCall
}

func (f *fakeClient) GetTable(ctx context.Context, dataset string, filters []Filter, fn RowFunc) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	f.calls = append(f.calls, fakeCall{dataset: dataset, filters: append([]Filter(nil), filters...)})
	resp, ok := f.responses[dataset]
	if !ok {
		return 0, nil
	}
	if resp.err != nil {
		return 0, resp.err
	}
	idx := make(map[string]int, len(resp.cols))
	for i, c := range resp.cols {
		idx[c] = i
	}
	var n int64
	for _, vals := range resp.rows {
		n++
		if err := fn(Row{cols: idx, vals: vals}); err != nil {
			return n, err
		}
	}
	return n, nil
}

func (f *fakeClient) callDatasets() []string {
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, c.dataset)
	}
	return out
}

// memStore is an in-memory syncStore with keyed last-wins merges, so
// net-new counting and idempotency match the SQL behavior.
type memStore struct {
	lastSync map[string]time.Time
	counts   map[string]int64

	merged      map[string]map[string][]any // dataset -> key -> row
	tickers     [][]any
	tickerNames []string

	recordSyncs []string // "SEP:6" history
	runs        []memRun

	failMerge   map[string]error // dataset -> error returned by sink.Close
	failTickers error
	mergeSeq    []string // datasets in NewMerge order
}

type memRun struct {
	id       int64
	dataset  string
	kind     string
	rows     int64
	err      string
	finished bool
}

func newMemStore() *memStore {
	return &memStore{
		lastSync:  map[string]time.Time{},
		counts:    map[string]int64{},
		merged:    map[string]map[string][]any{},
		failMerge: map[string]error{},
	}
}

func (m *memStore) Watermark(_ context.Context, dataset string) (time.Time, int64, bool, error) {
	ts, ok := m.lastSync[dataset]
	return ts, m.counts[dataset], ok, nil
}

func (m *memStore) RecordSync(_ context.Context, dataset string, rowCount int64) error {
	m.lastSync[dataset] = time.Now()
	m.counts[dataset] = rowCount
	m.recordSyncs = append(m.recordSyncs, fmt.Sprintf("%s:%d", dataset, rowCount))
	return nil
}

// mergeKey reproduces the dataset dedup keys (spec §6).
func mergeKey(dataset string, row []any) string {
	switch dataset {
	case DatasetSEP, DatasetSFP:
		return fmt.Sprint(row[0], "|", row[1], "|", row[2]) // ticker, ts, source
	case DatasetSF1:
		return fmt.Sprint(row[0], "|", row[3], "|", row[1]) // ticker, datekey, dimension
	case DatasetEvents:
		return fmt.Sprint(row[0], "|", row[1], "|", row[2]) // ticker, date, eventcodes
	}
	return fmt.Sprint(row)
}

type memSink struct {
	store   *memStore
	dataset string
	rows    [][]any
}

func (s *memSink) Add(_ context.Context, row []any) error {
	s.rows = append(s.rows, row)
	return nil
}

func (s *memSink) Close(_ context.Context) (int64, error) {
	if err := s.store.failMerge[s.dataset]; err != nil {
		return 0, err
	}
	tbl := s.store.merged[s.dataset]
	if tbl == nil {
		tbl = map[string][]any{}
		s.store.merged[s.dataset] = tbl
	}
	var added int64
	for _, row := range s.rows {
		k := mergeKey(s.dataset, row)
		if _, exists := tbl[k]; !exists {
			added++ // net-new keys only; revisions overwrite silently
		}
		tbl[k] = row
	}
	return added, nil
}

func (s *memSink) Abort() {}

func (m *memStore) NewMerge(_ context.Context, dataset string) (rowSink, error) {
	m.mergeSeq = append(m.mergeSeq, dataset)
	return &memSink{store: m, dataset: dataset}, nil
}

func (m *memStore) OverwriteTickers(_ context.Context, rows [][]any) (int64, error) {
	if m.failTickers != nil {
		return 0, m.failTickers
	}
	m.tickers = rows
	m.tickerNames = m.tickerNames[:0]
	for _, r := range rows {
		m.tickerNames = append(m.tickerNames, r[0].(string))
	}
	return int64(len(rows)), nil
}

func (m *memStore) ListTickers(_ context.Context) ([]string, error) {
	return append([]string(nil), m.tickerNames...), nil
}

func (m *memStore) StartRun(_ context.Context, dataset, kind string) (int64, error) {
	id := int64(len(m.runs) + 1)
	m.runs = append(m.runs, memRun{id: id, dataset: dataset, kind: kind})
	return id, nil
}

func (m *memStore) FinishRun(_ context.Context, runID, rows int64, runErr string) error {
	for i := range m.runs {
		if m.runs[i].id == runID {
			m.runs[i].rows = rows
			m.runs[i].err = runErr
			m.runs[i].finished = true
			return nil
		}
	}
	return fmt.Errorf("unknown run %d", runID)
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

func sepRows(tickers ...string) fakeResponse {
	rows := make([][]any, 0, len(tickers))
	for _, tk := range tickers {
		rows = append(rows, []any{tk, "2026-06-10", 10.0, 11.0, 9.0, 10.5, 1000.0, 10.5, 10.5, "2026-06-11"})
	}
	return fakeResponse{cols: sepAPICols, rows: rows}
}

func tickersUniverseResponse() fakeResponse {
	// The test_writer_tickers.py 7-row fixture: AAPL, ACWX, DEAD, MSFT, SPY
	// survive; PREF and a delisted ETF are dropped. Deliberately unsorted.
	mk := func(tk, table, category, isdel string) []any {
		return []any{tk, tk + " Inc", "NYSE", isdel, category, nil, nil, table, "2010-01-04", nil, nil}
	}
	return fakeResponse{
		cols: tickersAPICols,
		rows: [][]any{
			mk("SPY", "SFP", "ETF", "N"),
			mk("MSFT", "SF1", "Domestic Common Stock Primary Class", "N"),
			mk("DEADETF", "SFP", "ETF", "Y"),
			mk("AAPL", "SF1", "Domestic Common Stock", "N"),
			mk("PREF", "SF1", "Domestic Preferred Stock", "N"),
			mk("DEAD", "SF1", "Domestic Common Stock", "Y"),
			mk("ACWX", "SFP", "ETF", "N"),
		},
	}
}

func sf1Response(n int) fakeResponse {
	cols := []string{"ticker", "dimension", "calendardate", "datekey", "reportperiod", "fiscalperiod", "lastupdated", "marketcap"}
	rows := make([][]any, 0, n)
	for i := 0; i < n; i++ {
		rows = append(rows, []any{"AAPL", "MRT", "2026-03-31", fmt.Sprintf("2026-05-%02d", i+1), nil, "Q1", nil, 3.4e12})
	}
	return fakeResponse{cols: cols, rows: rows}
}

func eventsResponse(n int) fakeResponse {
	cols := []string{"ticker", "date", "eventcodes"}
	rows := make([][]any, 0, n)
	for i := 0; i < n; i++ {
		rows = append(rows, []any{"AAPL", fmt.Sprintf("2026-05-%02d", i+1), "22"})
	}
	return fakeResponse{cols: cols, rows: rows}
}

// newTestSyncer wires a Syncer over the fakes with a frozen clock.
// "Now" is 2026-06-12 12:00 ET (a Friday); T-1 = Thu 2026-06-11.
func newTestSyncer(t *testing.T, fc *fakeClient, ms *memStore, opts ...SyncerOption) *Syncer {
	t.Helper()
	cal, err := calendar.NewNYSE()
	require.NoError(t, err)
	now := time.Date(2026, time.June, 12, 12, 0, 0, 0, cal.Location())
	all := append([]SyncerOption{withStore(ms), withClock(func() time.Time { return now })}, opts...)
	s, err := NewSyncer(nil, fc, cal, all...)
	require.NoError(t, err)
	return s
}

// lastSyncAt sets a watermark at noon ET of the given date.
func lastSyncAt(t *testing.T, ms *memStore, dataset string, y int, mo time.Month, d int) {
	t.Helper()
	cal, err := calendar.NewNYSE()
	require.NoError(t, err)
	ms.lastSync[dataset] = time.Date(y, mo, d, 12, 0, 0, 0, cal.Location())
}

// ---------------------------------------------------------------------------
// EnsureFresh
// ---------------------------------------------------------------------------

func TestEnsureFreshNotBootstrapped(t *testing.T) {
	fc := &fakeClient{responses: map[string]fakeResponse{}}
	ms := newMemStore()
	s := newTestSyncer(t, fc, ms)

	report, err := s.EnsureFresh(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "not-bootstrapped", report.SkippedReason)
	assert.False(t, report.DidWork())
	assert.Empty(t, fc.calls, "never auto-bootstraps (spec §8.2 step 2)")
}

func TestEnsureFreshAlreadyFresh(t *testing.T) {
	fc := &fakeClient{responses: map[string]fakeResponse{}}
	ms := newMemStore()
	// Last sync today (NY): start = today > target = T-1 -> no days.
	lastSyncAt(t, ms, DatasetSEP, 2026, time.June, 12)
	s := newTestSyncer(t, fc, ms)

	report, err := s.EnsureFresh(context.Background())
	require.NoError(t, err)
	assert.Empty(t, report.SkippedReason)
	assert.False(t, report.DidWork())
	assert.Zero(t, report.DaysAttempted)
	assert.Empty(t, fc.calls)
}

func TestEnsureFreshFullFlowOracle(t *testing.T) {
	// Watermark Mon 2026-06-08 -> trading days Mon 8, Tue 9, Wed 10, Thu 11
	// (4 days; target Thu = T-1 of Fri 12).
	fc := &fakeClient{responses: map[string]fakeResponse{
		"SHARADAR/SEP":     sepRows("AAPL", "MSFT"),
		"SHARADAR/SFP":     sepRows("SPY"),
		"SHARADAR/TICKERS": tickersUniverseResponse(),
		"SHARADAR/SF1":     sf1Response(5),
		"SHARADAR/EVENTS":  eventsResponse(3),
	}}
	ms := newMemStore()
	lastSyncAt(t, ms, DatasetSEP, 2026, time.June, 8)
	ms.counts[DatasetSEP] = 100
	ms.counts[DatasetSFP] = 50
	ms.counts[DatasetSF1] = 1000
	ms.counts[DatasetEvents] = 500
	lastSyncAt(t, ms, DatasetSFP, 2026, time.June, 8)
	s := newTestSyncer(t, fc, ms)

	report, err := s.EnsureFresh(context.Background())
	require.NoError(t, err)
	require.Empty(t, report.Errors)
	assert.True(t, report.DidWork())
	assert.Equal(t, 4, report.DaysAttempted)
	assert.Equal(t, 4, report.DaysSucceeded)

	// Call order: SEP/SFP interleaved per day, then exactly one TICKERS,
	// SF1, EVENTS (test_catchup.py:102-141 ordering oracle).
	assert.Equal(t, []string{
		"SHARADAR/SEP", "SHARADAR/SFP",
		"SHARADAR/SEP", "SHARADAR/SFP",
		"SHARADAR/SEP", "SHARADAR/SFP",
		"SHARADAR/SEP", "SHARADAR/SFP",
		"SHARADAR/TICKERS", "SHARADAR/SF1", "SHARADAR/EVENTS",
	}, fc.callDatasets())

	// Per-day single-day date filters on SEP (asof.gte == asof.lte).
	assert.Equal(t, DateRangeFilters("2026-06-08", "2026-06-08"), fc.calls[0].filters)
	assert.Equal(t, DateRangeFilters("2026-06-11", "2026-06-11"), fc.calls[6].filters)

	// The fake returns identical (ticker, date) rows every day, so only
	// day 1 adds net-new keys — the idempotent-merge re-run oracle.
	assert.Equal(t, int64(2), report.RowsAdded[DatasetSEP])
	assert.Equal(t, int64(1), report.RowsAdded[DatasetSFP])
	assert.Equal(t, int64(5), report.RowsAdded[DatasetTickers], "filtered universe count")
	assert.Equal(t, int64(5), report.RowsAdded[DatasetSF1])
	assert.Equal(t, int64(3), report.RowsAdded[DatasetEvents])

	// TICKERS filtered + sorted: survivors of the 7-row fixture.
	assert.Equal(t, []string{"AAPL", "ACWX", "DEAD", "MSFT", "SPY"}, ms.tickerNames)

	// SF1/EVENTS driven by the reloaded ticker list (single 500-batch).
	sf1Call := fc.calls[9]
	require.Equal(t, "SHARADAR/SF1", sf1Call.dataset)
	var tickerFilter string
	for _, f := range sf1Call.filters {
		if f.Key == "ticker" {
			tickerFilter = f.Value
		}
	}
	assert.Equal(t, "AAPL,ACWX,DEAD,MSFT,SPY", tickerFilter)

	// Watermark accumulation: SEP = 100 + 2 net-new (then +0 on later
	// days); TICKERS absolute; SF1/EVENTS old + n (spec §5).
	assert.Contains(t, ms.recordSyncs, "SEP:102")
	assert.Contains(t, ms.recordSyncs, "SFP:51")
	assert.Contains(t, ms.recordSyncs, "TICKERS:5")
	assert.Contains(t, ms.recordSyncs, "SF1:1005")
	assert.Contains(t, ms.recordSyncs, "EVENTS:503")
	assert.Equal(t, int64(102), ms.counts[DatasetSEP])

	// Audit rows: one finished ok run per dataset.
	require.Len(t, ms.runs, 5)
	for _, r := range ms.runs {
		assert.True(t, r.finished, "%s run not finished", r.dataset)
		assert.Equal(t, runKindCatchup, r.kind)
		assert.Empty(t, r.err)
	}
}

func TestEnsureFreshIncrementalSF1Filter(t *testing.T) {
	fc := &fakeClient{responses: map[string]fakeResponse{
		"SHARADAR/TICKERS": tickersUniverseResponse(),
	}}
	ms := newMemStore()
	lastSyncAt(t, ms, DatasetSEP, 2026, time.June, 11)
	lastSyncAt(t, ms, DatasetSF1, 2026, time.June, 11)
	// EVENTS never synced -> full fetch (no lastupdated filter).
	s := newTestSyncer(t, fc, ms)

	_, err := s.EnsureFresh(context.Background())
	require.NoError(t, err)

	var sf1Filters, eventsFilters []Filter
	for _, c := range fc.calls {
		switch c.dataset {
		case "SHARADAR/SF1":
			sf1Filters = c.filters
		case "SHARADAR/EVENTS":
			eventsFilters = c.filters
		}
	}
	assert.Contains(t, sf1Filters, LastUpdatedGTEFilter("2026-06-11"),
		"synced SF1 uses the lastupdated.gte watermark (spec §6.6 IMPROVE)")
	for _, f := range eventsFilters {
		assert.NotEqual(t, "lastupdated.gte", f.Key, "unsynced EVENTS falls back to full fetch")
	}
}

func TestEnsureFreshFullRefetchOptionDisablesFilter(t *testing.T) {
	fc := &fakeClient{responses: map[string]fakeResponse{
		"SHARADAR/TICKERS": tickersUniverseResponse(),
	}}
	ms := newMemStore()
	lastSyncAt(t, ms, DatasetSEP, 2026, time.June, 11)
	lastSyncAt(t, ms, DatasetSF1, 2026, time.June, 11)
	s := newTestSyncer(t, fc, ms, WithFullRefetch())

	_, err := s.EnsureFresh(context.Background())
	require.NoError(t, err)
	for _, c := range fc.calls {
		for _, f := range c.filters {
			assert.NotEqual(t, "lastupdated.gte", f.Key, "WithFullRefetch reproduces the original full pull")
		}
	}
}

func TestEnsureFreshWarnAndContinueOnDayFailure(t *testing.T) {
	fc := &fakeClient{responses: map[string]fakeResponse{
		"SHARADAR/SEP":     {err: errors.New("HTTP 503 from upstream")},
		"SHARADAR/SFP":     sepRows("SPY"),
		"SHARADAR/TICKERS": tickersUniverseResponse(),
	}}
	ms := newMemStore()
	lastSyncAt(t, ms, DatasetSEP, 2026, time.June, 10) // Wed -> days Wed, Thu
	s := newTestSyncer(t, fc, ms)

	report, err := s.EnsureFresh(context.Background())
	require.NoError(t, err, "per-step failures never raise (spec §8.2)")
	assert.Equal(t, 2, report.DaysAttempted)
	assert.Equal(t, 0, report.DaysSucceeded, "a day succeeds only when both SEP and SFP succeed")
	assert.Equal(t, int64(1), report.RowsAdded[DatasetSFP], "SFP still ran after SEP failures")

	require.Len(t, report.Errors, 2)
	assert.Contains(t, report.Errors[0], "SEP 2026-06-10:")
	assert.Contains(t, report.Errors[1], "SEP 2026-06-11:")

	// SEP audit row carries the error; SFP is ok.
	for _, r := range ms.runs {
		switch r.dataset {
		case DatasetSEP:
			assert.NotEmpty(t, r.err)
		case DatasetSFP:
			assert.Empty(t, r.err)
		}
	}
}

func TestEnsureFreshTickersFailureDoesNotBlockSF1Events(t *testing.T) {
	fc := &fakeClient{responses: map[string]fakeResponse{
		"SHARADAR/SEP":     sepRows("AAPL"),
		"SHARADAR/SFP":     sepRows("SPY"),
		"SHARADAR/TICKERS": {err: errors.New("HTTP 500")},
		"SHARADAR/SF1":     sf1Response(2),
		"SHARADAR/EVENTS":  eventsResponse(1),
	}}
	ms := newMemStore()
	lastSyncAt(t, ms, DatasetSEP, 2026, time.June, 11)
	ms.tickerNames = []string{"AAPL", "MSFT"} // previous stored universe
	s := newTestSyncer(t, fc, ms)

	report, err := s.EnsureFresh(context.Background())
	require.NoError(t, err)
	require.NotEmpty(t, report.Errors)
	assert.Contains(t, report.Errors[0], "TICKERS:")

	// SF1/EVENTS still ran with the previous ticker list (spec §8.2 step 8).
	datasets := fc.callDatasets()
	assert.Contains(t, datasets, "SHARADAR/SF1")
	assert.Contains(t, datasets, "SHARADAR/EVENTS")
	assert.Equal(t, int64(2), report.RowsAdded[DatasetSF1])
}

func TestEnsureFreshEmptyTickerListSkipsSF1Events(t *testing.T) {
	fc := &fakeClient{responses: map[string]fakeResponse{
		"SHARADAR/SEP":     sepRows("AAPL"),
		"SHARADAR/SFP":     sepRows("SPY"),
		"SHARADAR/TICKERS": {cols: tickersAPICols, rows: nil}, // empty universe
	}}
	ms := newMemStore()
	lastSyncAt(t, ms, DatasetSEP, 2026, time.June, 11)
	s := newTestSyncer(t, fc, ms)

	report, err := s.EnsureFresh(context.Background())
	require.NoError(t, err)
	assert.Contains(t, report.Errors, "TICKERS list empty — skipping SF1 / EVENTS")
	for _, ds := range fc.callDatasets() {
		assert.NotEqual(t, "SHARADAR/SF1", ds)
		assert.NotEqual(t, "SHARADAR/EVENTS", ds)
	}
}

func TestEnsureFreshEmptyDayTouchesNothing(t *testing.T) {
	fc := &fakeClient{responses: map[string]fakeResponse{
		// SEP/SFP return zero rows (holiday-ish day): merge must not run.
		"SHARADAR/SEP":     {cols: sepAPICols},
		"SHARADAR/SFP":     {cols: sepAPICols},
		"SHARADAR/TICKERS": tickersUniverseResponse(),
	}}
	ms := newMemStore()
	lastSyncAt(t, ms, DatasetSEP, 2026, time.June, 11)
	s := newTestSyncer(t, fc, ms)

	report, err := s.EnsureFresh(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(0), report.RowsAdded[DatasetSEP])
	assert.Empty(t, ms.merged[DatasetSEP], "empty day returns 0 without touching the table (spec §6.3)")
	assert.Equal(t, 1, report.DaysSucceeded)
}

func TestEnsureFreshContextCancellation(t *testing.T) {
	fc := &fakeClient{responses: map[string]fakeResponse{}}
	ms := newMemStore()
	lastSyncAt(t, ms, DatasetSEP, 2026, time.June, 11)
	s := newTestSyncer(t, fc, ms)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	report, err := s.EnsureFresh(ctx)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	require.NotNil(t, report, "partial report returned alongside the error")
}

func TestSyncBarsDayIdempotent(t *testing.T) {
	fc := &fakeClient{responses: map[string]fakeResponse{
		"SHARADAR/SEP": sepRows("AAPL", "MSFT"),
	}}
	ms := newMemStore()
	s := newTestSyncer(t, fc, ms)

	asof := calendar.NewDate(2026, time.June, 10)
	n1, err := s.syncBarsDay(context.Background(), DatasetSEP, asof)
	require.NoError(t, err)
	assert.Equal(t, int64(2), n1)

	// Same-day re-run with identical data: net-new = 0, rows preserved
	// (idempotency oracle, spec §6 step 3 / test_writer_sep.py:80-103).
	n2, err := s.syncBarsDay(context.Background(), DatasetSEP, asof)
	require.NoError(t, err)
	assert.Equal(t, int64(0), n2)
	assert.Len(t, ms.merged[DatasetSEP], 2)
}

// ---------------------------------------------------------------------------
// Bootstrap
// ---------------------------------------------------------------------------

func TestBootstrapOrderAndQuarterChunks(t *testing.T) {
	fc := &fakeClient{responses: map[string]fakeResponse{
		"SHARADAR/TICKERS": tickersUniverseResponse(),
		"SHARADAR/SEP":     sepRows("AAPL"),
		"SHARADAR/SFP":     sepRows("SPY"),
		"SHARADAR/SF1":     sf1Response(4),
		"SHARADAR/EVENTS":  eventsResponse(2),
	}}
	ms := newMemStore()
	s := newTestSyncer(t, fc, ms)

	rows, err := s.Bootstrap(context.Background(), BootstrapOptions{
		Start: calendar.NewDate(2023, time.January, 1),
		End:   calendar.NewDate(2023, time.December, 31),
	})
	require.NoError(t, err)

	// Dataset order TICKERS -> SEP -> SFP -> SF1 -> EVENTS (spec §9), with
	// SEP/SFP in exactly 4 quarterly chunks (spec §6.2 oracle).
	assert.Equal(t, []string{
		"SHARADAR/TICKERS",
		"SHARADAR/SEP", "SHARADAR/SEP", "SHARADAR/SEP", "SHARADAR/SEP",
		"SHARADAR/SFP", "SHARADAR/SFP", "SHARADAR/SFP", "SHARADAR/SFP",
		"SHARADAR/SF1", "SHARADAR/EVENTS",
	}, fc.callDatasets())

	assert.Equal(t, DateRangeFilters("2023-01-01", "2023-03-31"), fc.calls[1].filters)
	assert.Equal(t, DateRangeFilters("2023-04-01", "2023-06-30"), fc.calls[2].filters)
	assert.Equal(t, DateRangeFilters("2023-07-01", "2023-09-30"), fc.calls[3].filters)
	assert.Equal(t, DateRangeFilters("2023-10-01", "2023-12-31"), fc.calls[4].filters)

	// Bootstrap never applies the lastupdated incremental filter.
	for _, c := range fc.calls {
		for _, f := range c.filters {
			assert.NotEqual(t, "lastupdated.gte", f.Key)
		}
	}

	assert.Equal(t, int64(5), rows[DatasetTickers])
	assert.Equal(t, int64(1), rows[DatasetSEP], "identical chunk rows dedup to one key")
	assert.Equal(t, int64(4), rows[DatasetSF1])

	// Bootstrap watermark semantics: row_count = rows written this run.
	assert.Contains(t, ms.recordSyncs, "TICKERS:5")
	assert.Contains(t, ms.recordSyncs, "SEP:1")
	assert.Contains(t, ms.recordSyncs, "SF1:4")

	for _, r := range ms.runs {
		assert.Equal(t, runKindBootstrap, r.kind)
		assert.True(t, r.finished)
	}
}

func TestBootstrapAbortsOnError(t *testing.T) {
	fc := &fakeClient{responses: map[string]fakeResponse{
		"SHARADAR/TICKERS": tickersUniverseResponse(),
		"SHARADAR/SEP":     {err: errors.New("HTTP 500")},
	}}
	ms := newMemStore()
	s := newTestSyncer(t, fc, ms)

	_, err := s.Bootstrap(context.Background(), BootstrapOptions{
		Start: calendar.NewDate(2023, time.January, 1),
		End:   calendar.NewDate(2023, time.March, 31),
	})
	require.Error(t, err, "bootstrap propagates API errors (CLI parity, spec §9)")
	assert.Contains(t, err.Error(), "bootstrap SEP")
	for _, ds := range fc.callDatasets() {
		assert.NotEqual(t, "SHARADAR/SFP", ds, "abort stops later datasets")
	}
}

func TestBootstrapEmptyUniverseAborts(t *testing.T) {
	fc := &fakeClient{responses: map[string]fakeResponse{
		"SHARADAR/TICKERS": {cols: tickersAPICols},
	}}
	ms := newMemStore()
	s := newTestSyncer(t, fc, ms)

	_, err := s.Bootstrap(context.Background(), BootstrapOptions{
		Start: calendar.NewDate(2023, time.January, 1),
		End:   calendar.NewDate(2023, time.March, 31),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ticker list empty")
}

func TestBootstrapInvalidRange(t *testing.T) {
	fc := &fakeClient{responses: map[string]fakeResponse{}}
	s := newTestSyncer(t, fc, newMemStore())
	_, err := s.Bootstrap(context.Background(), BootstrapOptions{
		Start: calendar.NewDate(2023, time.June, 2),
		End:   calendar.NewDate(2023, time.June, 1),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "before start")
}

func TestBootstrapTickerSmokePath(t *testing.T) {
	fc := &fakeClient{responses: map[string]fakeResponse{
		"SHARADAR/TICKERS": tickersUniverseResponse(),
		"SHARADAR/SEP":     sepRows("AAPL"),
		"SHARADAR/SFP":     {cols: sepAPICols},
		"SHARADAR/SF1":     sf1Response(1),
		"SHARADAR/EVENTS":  eventsResponse(1),
	}}
	ms := newMemStore()
	s := newTestSyncer(t, fc, ms)

	_, err := s.Bootstrap(context.Background(), BootstrapOptions{
		Start:   calendar.NewDate(2023, time.January, 1),
		End:     calendar.NewDate(2023, time.March, 31),
		Tickers: []string{"AAPL", "SPY"},
	})
	require.NoError(t, err)

	// SEP chunk calls carry the ticker narrow; SF1/EVENTS use the explicit
	// list instead of the stored universe.
	var sawSEPTickers, sawSF1Tickers bool
	for _, c := range fc.calls {
		for _, f := range c.filters {
			if f.Key == "ticker" && f.Value == "AAPL,SPY" {
				switch c.dataset {
				case "SHARADAR/SEP":
					sawSEPTickers = true
				case "SHARADAR/SF1":
					sawSF1Tickers = true
				}
			}
		}
	}
	assert.True(t, sawSEPTickers)
	assert.True(t, sawSF1Tickers)
}

// ---------------------------------------------------------------------------
// Misc
// ---------------------------------------------------------------------------

func TestNewSyncerValidation(t *testing.T) {
	cal, err := calendar.NewNYSE()
	require.NoError(t, err)
	_, err = NewSyncer(nil, nil, cal)
	require.Error(t, err)
	_, err = NewSyncer(nil, &fakeClient{}, nil)
	require.Error(t, err)
	_, err = NewSyncer(nil, &fakeClient{}, cal) // nil pool without store
	require.Error(t, err)
}

func TestCatchupReportErrorsFormat(t *testing.T) {
	// Error strings follow the Python "DATASET DATE: err" shape so log
	// scrapers keep working.
	fc := &fakeClient{responses: map[string]fakeResponse{
		"SHARADAR/SEP":     {err: errors.New("boom")},
		"SHARADAR/SFP":     {err: errors.New("boom")},
		"SHARADAR/TICKERS": {err: errors.New("boom")},
	}}
	ms := newMemStore()
	lastSyncAt(t, ms, DatasetSEP, 2026, time.June, 11)
	s := newTestSyncer(t, fc, ms)

	report, err := s.EnsureFresh(context.Background())
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(report.Errors), 3)
	assert.True(t, strings.HasPrefix(report.Errors[0], "SEP 2026-06-11: "))
	assert.True(t, strings.HasPrefix(report.Errors[1], "SFP 2026-06-11: "))
	assert.True(t, strings.HasPrefix(report.Errors[2], "TICKERS: "))
}
