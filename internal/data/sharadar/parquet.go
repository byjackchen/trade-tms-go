package sharadar

// parquet.go is the thin arrow-go wrapper: open a parquet file, stream
// record batches under a context, and read typed cells by column name with
// pandas-compatible coercions (timestamp[any unit]/date32/date64 -> tz-naive
// midnight semantics, dictionary-encoded strings, NULL handling).

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/arrow-go/v18/parquet/file"
	"github.com/apache/arrow-go/v18/parquet/pqarrow"
)

// arrowBatchSize is the row count per arrow record batch when streaming a
// parquet file. Bounded so a single batch of the widest table (SF1, 112
// columns) stays in the tens of megabytes.
const arrowBatchSize = 8192

// scanParquet streams a parquet file record-batch by record-batch, invoking
// fn for each batch. It owns open/close/release lifecycles and honors ctx
// cancellation between batches. fn must not retain the record after return.
// columns optionally projects the read to the named fields (nil = all);
// names absent from the file are ignored, so callers can request optional
// columns (e.g. the never-present TICKERS delistedate).
func scanParquet(ctx context.Context, path string, columns []string, fn func(rec arrow.Record) error) error {
	rdr, err := file.OpenParquetFile(path, false)
	if err != nil {
		return fmt.Errorf("sharadar: opening parquet %s: %w", path, err)
	}
	defer rdr.Close()

	fr, err := pqarrow.NewFileReader(rdr, pqarrow.ArrowReadProperties{BatchSize: arrowBatchSize}, memory.DefaultAllocator)
	if err != nil {
		return fmt.Errorf("sharadar: arrow reader for %s: %w", path, err)
	}

	var colIndices []int
	if columns != nil {
		schema, err := fr.Schema()
		if err != nil {
			return fmt.Errorf("sharadar: schema of %s: %w", path, err)
		}
		want := make(map[string]struct{}, len(columns))
		for _, c := range columns {
			want[strings.ToLower(c)] = struct{}{}
		}
		for i := 0; i < schema.NumFields(); i++ {
			if _, ok := want[strings.ToLower(schema.Field(i).Name)]; ok {
				colIndices = append(colIndices, i)
			}
		}
		if len(colIndices) == 0 {
			return fmt.Errorf("sharadar: %s has none of the requested columns %v", path, columns)
		}
	}

	rr, err := fr.GetRecordReader(ctx, colIndices, nil)
	if err != nil {
		return fmt.Errorf("sharadar: record reader for %s: %w", path, err)
	}
	defer rr.Release()

	for rr.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fn(rr.Record()); err != nil {
			return err
		}
	}
	if err := rr.Err(); err != nil {
		return fmt.Errorf("sharadar: reading %s: %w", path, err)
	}
	return nil
}

// colmap maps lower-cased column names to field indices; missing columns
// resolve to -1 via idx().
type colmap map[string]int

func colsOf(schema *arrow.Schema) colmap {
	m := make(colmap, schema.NumFields())
	for i := 0; i < schema.NumFields(); i++ {
		m[strings.ToLower(schema.Field(i).Name)] = i
	}
	return m
}

func (c colmap) idx(name string) int {
	if i, ok := c[name]; ok {
		return i
	}
	return -1
}

// stringCell returns the string value at (col,row), or ok=false when the
// column is absent or the value is NULL. Handles plain, large and
// dictionary-encoded UTF-8 columns.
func stringCell(rec arrow.Record, col, row int) (string, bool) {
	if col < 0 {
		return "", false
	}
	arr := rec.Column(col)
	if arr.IsNull(row) {
		return "", false
	}
	switch a := arr.(type) {
	case *array.String:
		return a.Value(row), true
	case *array.LargeString:
		return a.Value(row), true
	case *array.Dictionary:
		switch d := a.Dictionary().(type) {
		case *array.String:
			return d.Value(a.GetValueIndex(row)), true
		case *array.LargeString:
			return d.Value(a.GetValueIndex(row)), true
		}
	}
	return "", false
}

// floatCell returns the float64 value at (col,row), or ok=false when the
// column is absent or the value is NULL. NaN is returned as-is with ok=true
// (NaN vs NULL is a meaningful distinction upstream; both map to SQL NULL at
// conversion time). Integer-typed columns are widened, so fixtures or future
// cache revisions that store volume as int round-trip.
func floatCell(rec arrow.Record, col, row int) (float64, bool) {
	if col < 0 {
		return 0, false
	}
	arr := rec.Column(col)
	if arr.IsNull(row) {
		return 0, false
	}
	switch a := arr.(type) {
	case *array.Float64:
		return a.Value(row), true
	case *array.Float32:
		return float64(a.Value(row)), true
	case *array.Int64:
		return float64(a.Value(row)), true
	case *array.Int32:
		return float64(a.Value(row)), true
	}
	return 0, false
}

// timeCell returns the temporal value at (col,row) as a UTC time, or
// ok=false when the column is absent or the value is NULL. The cache stores
// tz-naive timestamps (spec §2.6); the naive wall-clock value is interpreted
// as UTC, which is exactly the engine-boundary tz_localize("UTC") semantics.
func timeCell(rec arrow.Record, col, row int) (time.Time, bool) {
	if col < 0 {
		return time.Time{}, false
	}
	arr := rec.Column(col)
	if arr.IsNull(row) {
		return time.Time{}, false
	}
	switch a := arr.(type) {
	case *array.Timestamp:
		tsType, ok := a.DataType().(*arrow.TimestampType)
		if !ok {
			return time.Time{}, false
		}
		return a.Value(row).ToTime(tsType.Unit).UTC(), true
	case *array.Date32:
		return a.Value(row).ToTime().UTC(), true
	case *array.Date64:
		return a.Value(row).ToTime().UTC(), true
	}
	return time.Time{}, false
}

// dateFlexCell reads a date that may be stored either as a temporal column
// (timestamp[ns] NaT for missing) or as a string column where "" means
// missing — the TICKERS lastpricedate dual representation the reader must
// accept (spec §2.5/§7.2). Unparseable strings coerce to missing, matching
// pd.to_datetime(errors="coerce").
func dateFlexCell(rec arrow.Record, col, row int) (time.Time, bool) {
	if col < 0 {
		return time.Time{}, false
	}
	if t, ok := timeCell(rec, col, row); ok {
		return t, true
	}
	s, ok := stringCell(rec, col, row)
	if !ok || strings.TrimSpace(s) == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{"2006-01-02", time.RFC3339, "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// utcMidnight truncates t to its UTC calendar date at 00:00:00 UTC — the
// instant equivalent of the cache's naive-midnight convention (spec §2.6).
func utcMidnight(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}
