package sharadar

// parquet_test.go proves the arrow-go read path end to end: tiny parquet
// fixtures are generated on the fly (same writer family the production cache
// uses) into a temp dir, then streamed through scanParquet and the row
// converters.

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/parquet"
	"github.com/apache/arrow-go/v18/parquet/compress"
	"github.com/apache/arrow-go/v18/parquet/pqarrow"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeFixture writes a single-record parquet file.
func writeFixture(t *testing.T, path string, rec arrow.Record) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	f, err := os.Create(path)
	require.NoError(t, err)
	defer f.Close()

	tbl := array.NewTableFromRecords(rec.Schema(), []arrow.Record{rec})
	defer tbl.Release()
	require.NoError(t, pqarrow.WriteTable(
		tbl, f, int64(rec.NumRows()),
		parquet.NewWriterProperties(parquet.WithCompression(compress.Codecs.Snappy)),
		pqarrow.DefaultWriterProps(),
	))
}

func TestScanParquetBarFixtureRoundTrip(t *testing.T) {
	dir := t.TempDir()
	rec := buildBarRecord(t)
	defer rec.Release()
	path := filepath.Join(dir, "SEP", "year=2024", "part-0.parquet")
	writeFixture(t, path, rec)

	var rows [][]any
	var failed int
	err := scanParquet(context.Background(), path, nil, func(rec arrow.Record) error {
		c := colsOf(rec.Schema())
		for i := 0; i < int(rec.NumRows()); i++ {
			row, _, err := convertBarRow(rec, c, i, DatasetSEP)
			if err != nil {
				failed++
				continue
			}
			rows = append(rows, row)
		}
		return nil
	})
	require.NoError(t, err)

	require.Len(t, rows, 2, "two convertible rows")
	assert.Equal(t, 1, failed, "the empty-ticker row fails conversion")

	// Row 0: full AAPL bar survives the parquet round trip bit-exactly.
	assert.Equal(t, []any{
		"AAPL", dayUTC("2024-01-02"), "SEP",
		int64(1_856_400), int64(1_869_500), int64(1_850_100), int64(1_862_800),
		int64(52_455_980), int64(1_854_041), int64(1_856_400), nil, dayUTC("2024-01-02"),
	}, rows[0])

	// Row 1: NaN close/volume written to parquet come back as NULLs.
	assert.Nil(t, rows[1][6])
	assert.Nil(t, rows[1][7])
	assert.Nil(t, rows[1][11])
}

func TestScanParquetColumnProjection(t *testing.T) {
	dir := t.TempDir()
	rec := buildBarRecord(t)
	defer rec.Release()
	path := filepath.Join(dir, "part-0.parquet")
	writeFixture(t, path, rec)

	var tickers []string
	err := scanParquet(context.Background(), path, []string{"ticker", "delistedate"}, func(rec arrow.Record) error {
		require.EqualValues(t, 1, rec.NumCols(), "projection keeps only existing requested columns")
		c := colsOf(rec.Schema())
		for i := 0; i < int(rec.NumRows()); i++ {
			if s, ok := stringCell(rec, c.idx("ticker"), i); ok {
				tickers = append(tickers, s)
			}
		}
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"AAPL", "GAPY", ""}, tickers)

	// Requesting only columns that do not exist is an error, not a silent
	// full-file scan.
	err = scanParquet(context.Background(), path, []string{"nope"}, func(arrow.Record) error { return nil })
	require.Error(t, err)
}

func TestScanParquetContextCancellation(t *testing.T) {
	dir := t.TempDir()
	rec := buildBarRecord(t)
	defer rec.Release()
	path := filepath.Join(dir, "part-0.parquet")
	writeFixture(t, path, rec)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := scanParquet(ctx, path, nil, func(arrow.Record) error {
		t.Fatal("callback must not run after cancellation")
		return nil
	})
	require.ErrorIs(t, err, context.Canceled)
}

func TestScanParquetEventsFixture(t *testing.T) {
	dir := t.TempDir()
	rec := buildEventRecord(t)
	defer rec.Release()
	path := filepath.Join(dir, "EVENTS", "ticker=AAPL.parquet")
	writeFixture(t, path, rec)

	var rows [][]any
	var failed int
	err := scanParquet(context.Background(), path, nil, func(rec arrow.Record) error {
		c := colsOf(rec.Schema())
		for i := 0; i < int(rec.NumRows()); i++ {
			row, _, err := convertEventRow(rec, c, i)
			if err != nil {
				failed++
				continue
			}
			rows = append(rows, row)
		}
		return nil
	})
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, 1, failed)
	assert.Equal(t, []any{"AAPL", dayUTC("2024-05-02"), "22|71"}, rows[0])
	assert.Equal(t, []any{"AAPL", dayUTC("2024-05-02"), "13"}, rows[1])
}

func TestScanParquetMissingFile(t *testing.T) {
	err := scanParquet(context.Background(), filepath.Join(t.TempDir(), "nope.parquet"), nil,
		func(arrow.Record) error { return nil })
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// Layout helpers
// ---------------------------------------------------------------------------

func TestYearPartitionsAndPerTickerFiles(t *testing.T) {
	root := t.TempDir()
	mk := func(rel string) {
		p := filepath.Join(root, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte("x"), 0o644))
	}
	mk("SEP/year=2024/part-0.parquet")
	mk("SEP/year=2020/part-0.parquet")
	mk("SEP/year=2020/part-1.parquet") // multi-part tolerated (spec §4)
	mk("SEP/year=bogus/part-0.parquet")
	mk("SF1/ticker=BRK.A.parquet") // raw symbol with '.' (spec §4)
	mk("SF1/ticker=AAPL.parquet")

	parts, err := yearPartitions(root, DatasetSEP)
	require.NoError(t, err)
	require.Len(t, parts, 3, "bogus year dir ignored")
	assert.Equal(t, 2020, parts[0].Year)
	assert.Equal(t, 2020, parts[1].Year)
	assert.Equal(t, 2024, parts[2].Year)

	files, err := perTickerFiles(root, DatasetSF1)
	require.NoError(t, err)
	require.Len(t, files, 2)
	assert.Equal(t, "AAPL", files[0].Ticker)
	assert.Equal(t, "BRK.A", files[1].Ticker)

	none, err := yearPartitions(root, DatasetSFP)
	require.NoError(t, err)
	assert.Empty(t, none)
}

func TestResolveCacheDir(t *testing.T) {
	dir := t.TempDir()

	got, err := ResolveCacheDir(dir, "")
	require.NoError(t, err)
	assert.Equal(t, dir, got)

	got, err = ResolveCacheDir("", dir)
	require.NoError(t, err)
	assert.Equal(t, dir, got)

	// Explicit wins over configured.
	other := t.TempDir()
	got, err = ResolveCacheDir(dir, other)
	require.NoError(t, err)
	assert.Equal(t, dir, got)

	_, err = ResolveCacheDir(filepath.Join(dir, "missing"), "")
	require.ErrorIs(t, err, ErrCacheDirNotFound)

	// A file (not a directory) is rejected.
	f := filepath.Join(dir, "f")
	require.NoError(t, os.WriteFile(f, nil, 0o644))
	_, err = ResolveCacheDir(f, "")
	require.ErrorIs(t, err, ErrCacheDirNotFound)
}

func TestVolumeNaNSurvivesParquetAsNull(t *testing.T) {
	// Regression guard for the float-volume contract (spec §2.1): a NaN
	// double written by pandas-equivalent writers must reach the DB layer as
	// SQL NULL, never 0 and never an error.
	dir := t.TempDir()
	rec := buildBarRecord(t)
	defer rec.Release()
	path := filepath.Join(dir, "p.parquet")
	writeFixture(t, path, rec)

	err := scanParquet(context.Background(), path, []string{"volume"}, func(rec arrow.Record) error {
		c := colsOf(rec.Schema())
		v, valid := floatCell(rec, c.idx("volume"), 1)
		assert.True(t, valid, "NaN is a present value at the arrow layer")
		assert.True(t, math.IsNaN(v))
		cell, warn := volumeCell(v, valid)
		assert.Nil(t, cell)
		assert.False(t, warn)
		return nil
	})
	require.NoError(t, err)
}
