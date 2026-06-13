package sharadar

import (
	"math"
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tsNaive is the tz-naive timestamp[ns] type the pandas-written cache uses.
var tsNaive = &arrow.TimestampType{Unit: arrow.Nanosecond}

func tsOf(t *testing.T, s string) arrow.Timestamp {
	t.Helper()
	tt, err := time.ParseInLocation("2006-01-02", s, time.UTC)
	require.NoError(t, err)
	v, err := arrow.TimestampFromTime(tt, arrow.Nanosecond)
	require.NoError(t, err)
	return v
}

func dayUTC(s string) time.Time {
	tt, _ := time.ParseInLocation("2006-01-02", s, time.UTC)
	return tt
}

// ---------------------------------------------------------------------------
// Scalar conversions
// ---------------------------------------------------------------------------

func TestPriceCell(t *testing.T) {
	tests := []struct {
		name   string
		v      float64
		valid  bool
		want   any
		nulled bool
	}{
		// The documented rounding: Decimal(str(x)).quantize(0.0001, HALF_EVEN).
		{"exact 4dp", 1.0005, true, int64(10005), false},
		{"two decimals", 123.45, true, int64(1_234_500), false},
		{"shortest-repr tie rounds down to even", 0.00005, true, int64(0), false},
		{"shortest-repr tie rounds up to even", 0.00015, true, int64(2), false},
		{"2.675 keeps its shortest repr (no PyRound)", 2.675, true, int64(26750), false},
		{"negative", -70.5, true, int64(-705000), false},
		{"sub-4dp rounds half-even", 12.34565, true, int64(123_456), false}, // str = "12.34565" -> 12.3456|5 tie, 6 even -> 12.3456
		{"null", 0, false, nil, false},
		{"NaN is faithful NULL, not a warning", math.NaN(), true, nil, false},
		{"+Inf unrepresentable", math.Inf(1), true, nil, true},
		{"beyond int64 1e-4 range", 1e15, true, nil, true},
		// Production-cache overflow pin (spec §2.1 [IMPROVE]): BINI's peak
		// back-adjusted price, the only real-data int64-at-1e-4 overflow
		// (3,479 cells, one ticker) — stored NULL + FieldsNulled-counted.
		{"BINI peak overflows -> NULL + counted", 1.4065e18, true, nil, true},
		{"price cap value fits", 17_014_118_346_046.0, true, int64(170_141_183_460_460_000), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, nulled := priceCell(tc.v, tc.valid)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.nulled, nulled)
		})
	}
}

func TestVolumeCell(t *testing.T) {
	tests := []struct {
		name   string
		v      float64
		valid  bool
		want   any
		nulled bool
	}{
		{"whole float", 1234.0, true, int64(1234), false},
		{"truncates toward zero like Python int()", 12.9, true, int64(12), false},
		{"negative fraction truncates to zero", -0.5, true, int64(0), false},
		{"zero", 0.0, true, int64(0), false},
		{"null", 0, false, nil, false},
		{"NaN tolerated as NULL (spec §2.1)", math.NaN(), true, nil, false},
		{"negative violates schema -> nulled", -5.0, true, nil, true},
		{"beyond int64 -> nulled", 9.3e18, true, nil, true},
		{"+Inf -> nulled", math.Inf(1), true, nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, nulled := volumeCell(tc.v, tc.valid)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.nulled, nulled)
		})
	}
}

func TestUTCMidnight(t *testing.T) {
	in := time.Date(2024, 3, 5, 13, 45, 12, 999, time.UTC)
	assert.Equal(t, time.Date(2024, 3, 5, 0, 0, 0, 0, time.UTC), utcMidnight(in))
	// Already-midnight values are unchanged (the cache normalizes on write).
	mid := dayUTC("2020-01-02")
	assert.Equal(t, mid, utcMidnight(mid))
}

// ---------------------------------------------------------------------------
// Record builders for row-converter tests
// ---------------------------------------------------------------------------

// buildBarRecord builds a 3-row SEP/SFP-shaped record:
//
//	row 0: normal AAPL bar
//	row 1: NaN volume + NaN close (must survive as NULLs)
//	row 2: empty ticker (row error)
func buildBarRecord(t *testing.T) arrow.Record {
	t.Helper()
	schema := arrow.NewSchema([]arrow.Field{
		{Name: "ticker", Type: arrow.BinaryTypes.String},
		{Name: "date", Type: tsNaive},
		{Name: "open", Type: arrow.PrimitiveTypes.Float64},
		{Name: "high", Type: arrow.PrimitiveTypes.Float64},
		{Name: "low", Type: arrow.PrimitiveTypes.Float64},
		{Name: "close", Type: arrow.PrimitiveTypes.Float64, Nullable: true},
		{Name: "volume", Type: arrow.PrimitiveTypes.Float64, Nullable: true},
		{Name: "closeadj", Type: arrow.PrimitiveTypes.Float64},
		{Name: "closeunadj", Type: arrow.PrimitiveTypes.Float64},
		{Name: "lastupdated", Type: tsNaive, Nullable: true},
	}, nil)

	b := array.NewRecordBuilder(memory.DefaultAllocator, schema)
	defer b.Release()

	b.Field(0).(*array.StringBuilder).AppendValues([]string{"AAPL", "GAPY", ""}, nil)
	b.Field(1).(*array.TimestampBuilder).AppendValues(
		[]arrow.Timestamp{tsOf(t, "2024-01-02"), tsOf(t, "2024-01-03"), tsOf(t, "2024-01-04")}, nil)
	b.Field(2).(*array.Float64Builder).AppendValues([]float64{185.64, 1.0005, 1}, nil)
	b.Field(3).(*array.Float64Builder).AppendValues([]float64{186.95, 2, 1}, nil)
	b.Field(4).(*array.Float64Builder).AppendValues([]float64{185.01, 0.5, 1}, nil)
	b.Field(5).(*array.Float64Builder).AppendValues([]float64{186.28, math.NaN(), 1}, nil)
	b.Field(6).(*array.Float64Builder).AppendValues([]float64{52455980, math.NaN(), 1}, nil)
	b.Field(7).(*array.Float64Builder).AppendValues([]float64{185.40411, 1.5, 1}, nil)
	b.Field(8).(*array.Float64Builder).AppendValues([]float64{185.64, 1.0005, 1}, nil)
	lu := b.Field(9).(*array.TimestampBuilder)
	lu.Append(tsOf(t, "2024-01-02"))
	lu.AppendNull()
	lu.Append(tsOf(t, "2024-01-04"))

	return b.NewRecord()
}

func TestConvertBarRow(t *testing.T) {
	rec := buildBarRecord(t)
	defer rec.Release()
	c := colsOf(rec.Schema())

	t.Run("normal row", func(t *testing.T) {
		row, nulled, err := convertBarRow(rec, c, 0, DatasetSEP)
		require.NoError(t, err)
		assert.Zero(t, nulled)
		require.Len(t, row, len(barColumns))
		assert.Equal(t, []any{
			"AAPL", dayUTC("2024-01-02"), "SEP",
			int64(1_856_400), int64(1_869_500), int64(1_850_100), int64(1_862_800),
			int64(52_455_980),
			int64(1_854_041), // 185.40411 -> half-even at 4dp -> 185.4041
			int64(1_856_400),
			nil, // dividends column absent in the cache
			dayUTC("2024-01-02"),
		}, row)
	})

	t.Run("NaN close and volume map to NULL without warnings", func(t *testing.T) {
		row, nulled, err := convertBarRow(rec, c, 1, DatasetSFP)
		require.NoError(t, err)
		assert.Zero(t, nulled)
		assert.Equal(t, "GAPY", row[0])
		assert.Equal(t, "SFP", row[2])
		assert.Equal(t, int64(10005), row[3]) // open 1.0005
		assert.Nil(t, row[6])                 // close NaN -> NULL
		assert.Nil(t, row[7])                 // volume NaN -> NULL
		assert.Nil(t, row[11])                // lastupdated NULL
	})

	t.Run("empty ticker skips the row", func(t *testing.T) {
		_, _, err := convertBarRow(rec, c, 2, DatasetSEP)
		require.ErrorIs(t, err, errSkipRow)
	})
}

func buildTickerRecord(t *testing.T) arrow.Record {
	t.Helper()
	schema := arrow.NewSchema([]arrow.Field{
		{Name: "ticker", Type: arrow.BinaryTypes.String},
		{Name: "name", Type: arrow.BinaryTypes.String, Nullable: true},
		{Name: "exchange", Type: arrow.BinaryTypes.String, Nullable: true},
		{Name: "isdelisted", Type: arrow.BinaryTypes.String},
		{Name: "category", Type: arrow.BinaryTypes.String, Nullable: true},
		{Name: "sector", Type: arrow.BinaryTypes.String, Nullable: true},
		{Name: "industry", Type: arrow.BinaryTypes.String, Nullable: true},
		{Name: "table", Type: arrow.BinaryTypes.String},
		{Name: "firstpricedate", Type: tsNaive, Nullable: true},
		// lastpricedate as a STRING column with "" = still active — the
		// alternate representation the reader must accept (spec §2.5).
		{Name: "lastpricedate", Type: arrow.BinaryTypes.String, Nullable: true},
	}, nil)

	b := array.NewRecordBuilder(memory.DefaultAllocator, schema)
	defer b.Release()

	b.Field(0).(*array.StringBuilder).AppendValues([]string{"AAPL", "DEAD", "WEIRD", "BADTBL"}, nil)
	b.Field(1).(*array.StringBuilder).AppendValues([]string{"Apple Inc", "Dead Co", "Weird Co", "Bad Co"}, nil)
	b.Field(2).(*array.StringBuilder).AppendValues([]string{"NASDAQ", "NYSE", "NYSE", "NYSE"}, nil)
	b.Field(3).(*array.StringBuilder).AppendValues([]string{"N", "Y", "maybe", "N"}, nil)
	b.Field(4).(*array.StringBuilder).AppendValues([]string{"Domestic Common Stock", "Domestic Common Stock", "Domestic Common Stock", "Domestic Common Stock"}, nil)
	sec := b.Field(5).(*array.StringBuilder)
	sec.Append("Technology")
	sec.AppendNull()
	sec.Append("X")
	sec.Append("X")
	b.Field(6).(*array.StringBuilder).AppendValues([]string{"Consumer Electronics", "None", "X", "X"}, nil)
	b.Field(7).(*array.StringBuilder).AppendValues([]string{"SF1", "SF1", "SF1", "SEP"}, nil)
	fp := b.Field(8).(*array.TimestampBuilder)
	fp.Append(tsOf(t, "1986-01-01"))
	fp.Append(tsOf(t, "2000-05-01"))
	fp.AppendNull()
	fp.AppendNull()
	b.Field(9).(*array.StringBuilder).AppendValues([]string{"", "2017-08-03", "", ""}, nil)

	return b.NewRecord()
}

func TestConvertTickerRow(t *testing.T) {
	rec := buildTickerRecord(t)
	defer rec.Release()
	c := colsOf(rec.Schema())

	t.Run("active ticker", func(t *testing.T) {
		row, nulled, err := convertTickerRow(rec, c, 0)
		require.NoError(t, err)
		assert.Zero(t, nulled)
		require.Len(t, row, len(tickerColumns))
		assert.Equal(t, []any{
			"AAPL", "Apple Inc", "NASDAQ", false, "Domestic Common Stock",
			"Technology", "Consumer Electronics", "SF1",
			dayUTC("1986-01-01"),
			nil, // lastpricedate "" -> still active -> NULL
			nil, // delistedate never present on disk -> NULL (spec Q2)
		}, row)
	})

	t.Run("delisted ticker with string lastpricedate", func(t *testing.T) {
		row, _, err := convertTickerRow(rec, c, 1)
		require.NoError(t, err)
		assert.Equal(t, true, row[3])
		assert.Nil(t, row[5]) // NULL sector survives as NULL
		assert.Equal(t, dayUTC("2017-08-03"), row[9])
	})

	t.Run("invalid isdelisted skips the row", func(t *testing.T) {
		_, _, err := convertTickerRow(rec, c, 2)
		require.ErrorIs(t, err, errSkipRow)
	})

	t.Run("non SF1/SFP table skips the row", func(t *testing.T) {
		_, _, err := convertTickerRow(rec, c, 3)
		require.ErrorIs(t, err, errSkipRow)
	})
}

// buildSF1Record builds a minimal SF1-shaped record carrying the key columns
// plus two metric columns (marketcap, revenue); the other 103 metrics are
// intentionally absent to prove missing columns map to NULL.
func buildSF1Record(t *testing.T) arrow.Record {
	t.Helper()
	schema := arrow.NewSchema([]arrow.Field{
		{Name: "ticker", Type: arrow.BinaryTypes.String},
		{Name: "dimension", Type: arrow.BinaryTypes.String},
		{Name: "calendardate", Type: tsNaive, Nullable: true},
		{Name: "datekey", Type: tsNaive},
		{Name: "reportperiod", Type: tsNaive, Nullable: true},
		{Name: "fiscalperiod", Type: arrow.BinaryTypes.String, Nullable: true},
		{Name: "lastupdated", Type: tsNaive, Nullable: true},
		{Name: "marketcap", Type: arrow.PrimitiveTypes.Float64, Nullable: true},
		{Name: "revenue", Type: arrow.PrimitiveTypes.Float64, Nullable: true},
	}, nil)

	b := array.NewRecordBuilder(memory.DefaultAllocator, schema)
	defer b.Release()

	b.Field(0).(*array.StringBuilder).AppendValues([]string{"AAPL", "AAPL"}, nil)
	b.Field(1).(*array.StringBuilder).AppendValues([]string{"MRT", "XXX"}, nil)
	b.Field(2).(*array.TimestampBuilder).AppendValues([]arrow.Timestamp{tsOf(t, "2024-03-31"), tsOf(t, "2024-03-31")}, nil)
	b.Field(3).(*array.TimestampBuilder).AppendValues([]arrow.Timestamp{tsOf(t, "2024-05-03"), tsOf(t, "2024-05-03")}, nil)
	b.Field(4).(*array.TimestampBuilder).AppendValues([]arrow.Timestamp{tsOf(t, "2024-03-30"), tsOf(t, "2024-03-30")}, nil)
	b.Field(5).(*array.StringBuilder).AppendValues([]string{"Q2", "Q2"}, nil)
	b.Field(6).(*array.TimestampBuilder).AppendValues([]arrow.Timestamp{tsOf(t, "2024-05-04"), tsOf(t, "2024-05-04")}, nil)
	mc := b.Field(7).(*array.Float64Builder)
	mc.Append(3.4e12) // raw USD, stays double (spec §2.3)
	mc.Append(1)
	rev := b.Field(8).(*array.Float64Builder)
	rev.Append(math.NaN())
	rev.Append(1)

	return b.NewRecord()
}

func TestConvertSF1Row(t *testing.T) {
	rec := buildSF1Record(t)
	defer rec.Release()
	c := colsOf(rec.Schema())

	row, nulled, err := convertSF1Row(rec, c, 0)
	require.NoError(t, err)
	assert.Zero(t, nulled)
	require.Len(t, row, len(sf1Columns))

	assert.Equal(t, "AAPL", row[0])
	assert.Equal(t, "MRT", row[1])
	assert.Equal(t, dayUTC("2024-03-31"), row[2])
	assert.Equal(t, dayUTC("2024-05-03"), row[3])
	assert.Equal(t, dayUTC("2024-03-30"), row[4])
	assert.Equal(t, "Q2", row[5])
	assert.Equal(t, dayUTC("2024-05-04"), row[6])

	byName := make(map[string]any, len(sf1Columns))
	for i, name := range sf1Columns {
		byName[name] = row[i]
	}
	assert.Equal(t, 3.4e12, byName["marketcap"]) // double passes through unrounded
	assert.Nil(t, byName["revenue"])             // NaN -> NULL
	assert.Nil(t, byName["assets"])              // absent column -> NULL
	assert.Nil(t, byName["workingcapital"])

	_, _, err = convertSF1Row(rec, c, 1)
	require.ErrorIs(t, err, errSkipRow, "unknown dimension must skip the row")
}

func buildEventRecord(t *testing.T) arrow.Record {
	t.Helper()
	schema := arrow.NewSchema([]arrow.Field{
		{Name: "ticker", Type: arrow.BinaryTypes.String},
		{Name: "date", Type: tsNaive},
		{Name: "eventcodes", Type: arrow.BinaryTypes.String, Nullable: true},
	}, nil)

	b := array.NewRecordBuilder(memory.DefaultAllocator, schema)
	defer b.Release()
	b.Field(0).(*array.StringBuilder).AppendValues([]string{"AAPL", "AAPL", "AAPL"}, nil)
	b.Field(1).(*array.TimestampBuilder).AppendValues(
		[]arrow.Timestamp{tsOf(t, "2024-05-02"), tsOf(t, "2024-05-02"), tsOf(t, "2024-05-03")}, nil)
	ec := b.Field(2).(*array.StringBuilder)
	ec.Append("22|71") // pipe-separated codes stored verbatim (spec §2.4)
	ec.Append("13")
	ec.AppendNull()
	return b.NewRecord()
}

func TestConvertEventRow(t *testing.T) {
	rec := buildEventRecord(t)
	defer rec.Release()
	c := colsOf(rec.Schema())

	row, nulled, err := convertEventRow(rec, c, 0)
	require.NoError(t, err)
	assert.Zero(t, nulled)
	assert.Equal(t, []any{"AAPL", dayUTC("2024-05-02"), "22|71"}, row)

	// Same-day second event with a different code string coexists (dedup key
	// is the whole row, spec §2.4).
	row, _, err = convertEventRow(rec, c, 1)
	require.NoError(t, err)
	assert.Equal(t, []any{"AAPL", dayUTC("2024-05-02"), "13"}, row)

	_, _, err = convertEventRow(rec, c, 2)
	require.ErrorIs(t, err, errSkipRow, "missing eventcodes must skip the row")
}

// ---------------------------------------------------------------------------
// Column-set sanity against the migration contract
// ---------------------------------------------------------------------------

func TestSF1ColumnSets(t *testing.T) {
	assert.Len(t, sf1MetricColumns, 105, "SF1 metric column count is pinned by migration 000002")
	assert.Len(t, sf1Columns, 112, "7 key columns + 105 metrics")

	seen := make(map[string]struct{}, len(sf1Columns))
	for _, c := range sf1Columns {
		_, dup := seen[c]
		assert.False(t, dup, "duplicate SF1 column %q", c)
		seen[c] = struct{}{}
	}
	// Spot-check the consumed columns (spec §2.3).
	for _, c := range []string{"ticker", "datekey", "dimension", "marketcap", "revenue"} {
		_, ok := seen[c]
		assert.True(t, ok, "missing SF1 column %q", c)
	}
}

func TestNormalizeTables(t *testing.T) {
	all, err := normalizeTables(nil)
	require.NoError(t, err)
	assert.Equal(t, DatasetOrder, all)

	all, err = normalizeTables([]string{"ALL"})
	require.NoError(t, err)
	assert.Equal(t, DatasetOrder, all)

	// Order is canonical regardless of input order (bootstrap order, spec §9).
	got, err := normalizeTables([]string{"events", "sep", "TICKERS"})
	require.NoError(t, err)
	assert.Equal(t, []string{DatasetTickers, DatasetSEP, DatasetEvents}, got)

	_, err = normalizeTables([]string{"nope"})
	require.Error(t, err)
}

func TestStagingPlans(t *testing.T) {
	bars := barsPlan()
	assert.Equal(t, barColumns, bars.columns)
	assert.Contains(t, bars.upsertSQL, "ON CONFLICT (ticker, ts, source) DO UPDATE SET")
	assert.Contains(t, bars.upsertSQL, "DISTINCT ON (ticker, ts, source)")
	assert.Contains(t, bars.upsertSQL, "seq DESC", "new rows win on key collision (spec §6 keep=last)")
	assert.NotContains(t, bars.upsertSQL, "ticker = EXCLUDED.ticker", "key columns must not be in the update set")

	tk := tickersPlan()
	assert.Equal(t, tickerColumns, tk.columns)
	assert.Contains(t, tk.upsertSQL, "ON CONFLICT (ticker) DO UPDATE SET")

	sf1 := sf1Plan()
	assert.Equal(t, sf1Columns, sf1.columns)
	assert.Contains(t, sf1.upsertSQL, "ON CONFLICT (ticker, datekey, dimension) DO UPDATE SET")

	ev := eventsPlan()
	assert.Equal(t, eventColumns, ev.columns)
	assert.Contains(t, ev.upsertSQL, "ON CONFLICT (ticker, event_date, eventcodes) DO NOTHING",
		"the whole events row is the key — revisions are impossible")
}
