package sharadar

// syncconvert_test.go pins the API-row -> SQL-row converters: same numeric
// semantics as the parquet path (1e-4 fixed-point prices, NaN/null -> NULL,
// UTC-midnight dates) plus the API-only representations (string dates,
// JSON nulls, delistdate per P1 locked decision 3).

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// apiRow builds a Row from parallel column/value slices.
func apiRow(cols []string, vals []any) Row {
	idx := make(map[string]int, len(cols))
	for i, c := range cols {
		idx[c] = i
	}
	return Row{cols: idx, vals: vals}
}

var sepAPICols = []string{"ticker", "date", "open", "high", "low", "close", "volume", "closeadj", "closeunadj", "lastupdated"}

func TestConvertBarAPIRow(t *testing.T) {
	r := apiRow(sepAPICols, []any{"AAPL", "2024-01-02", 185.0, 186.5, 184.25, 185.75, 1000000.0, 185.75, 185.75, "2024-01-03"})
	row, nulled, err := convertBarAPIRow(r, DatasetSEP)
	require.NoError(t, err)
	assert.Equal(t, 0, nulled)
	require.Len(t, row, len(barColumns))

	assert.Equal(t, "AAPL", row[0])
	assert.Equal(t, time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC), row[1])
	assert.Equal(t, "SEP", row[2])
	assert.Equal(t, int64(1850000), row[3], "open: 185.0 -> 1e-4 fixed point")
	assert.Equal(t, int64(1857500), row[6], "close")
	assert.Equal(t, int64(1000000), row[7], "volume truncated int")
	assert.Nil(t, row[10], "dividends column absent -> NULL")
	assert.Equal(t, time.Date(2024, 1, 3, 0, 0, 0, 0, time.UTC), row[11])
}

func TestConvertBarAPIRowNullsAndErrors(t *testing.T) {
	// JSON null prices/volume -> SQL NULL, no warning (faithful NULL).
	r := apiRow(sepAPICols, []any{"NANY", "2024-01-02", nil, 1.0, 0.5, 0.75, nil, 0.75, 0.75, nil})
	row, nulled, err := convertBarAPIRow(r, DatasetSFP)
	require.NoError(t, err)
	assert.Equal(t, 0, nulled)
	assert.Nil(t, row[3], "null open")
	assert.Nil(t, row[7], "null volume")
	assert.Nil(t, row[11], "null lastupdated")
	assert.Equal(t, "SFP", row[2])

	// Missing ticker / date -> errSkipRow.
	_, _, err = convertBarAPIRow(apiRow(sepAPICols, []any{nil, "2024-01-02", 1.0, 1.0, 1.0, 1.0, 1.0, 1.0, 1.0, nil}), DatasetSEP)
	assert.ErrorIs(t, err, errSkipRow)
	_, _, err = convertBarAPIRow(apiRow(sepAPICols, []any{"AAPL", nil, 1.0, 1.0, 1.0, 1.0, 1.0, 1.0, 1.0, nil}), DatasetSEP)
	assert.ErrorIs(t, err, errSkipRow)

	// Unrepresentable price (beyond int64 at 1e-4) -> NULL + counted.
	r = apiRow(sepAPICols, []any{"BINI", "2024-01-02", 1.4065e18, 1.0, 1.0, 1.0, 1.0, 1.0, 1.0, nil})
	row, nulled, err = convertBarAPIRow(r, DatasetSEP)
	require.NoError(t, err)
	assert.Equal(t, 1, nulled)
	assert.Nil(t, row[3])

	// Negative volume violates the schema CHECK -> NULL + counted.
	r = apiRow(sepAPICols, []any{"NEG", "2024-01-02", 1.0, 1.0, 1.0, 1.0, -5.0, 1.0, 1.0, nil})
	row, nulled, err = convertBarAPIRow(r, DatasetSEP)
	require.NoError(t, err)
	assert.Equal(t, 1, nulled)
	assert.Nil(t, row[7])
}

var tickersAPICols = []string{"ticker", "name", "exchange", "isdelisted", "category", "sector",
	"industry", "table", "firstpricedate", "lastpricedate", "delistdate"}

func TestConvertTickerAPIRow(t *testing.T) {
	r := apiRow(tickersAPICols, []any{"AAPL", "Apple Inc", "NASDAQ", "N", "Domestic Common Stock",
		"Technology", "Consumer Electronics", "SF1", "1986-01-01", nil, nil})
	row, nulled, err := convertTickerAPIRow(r)
	require.NoError(t, err)
	assert.Equal(t, 0, nulled)
	require.Len(t, row, len(tickerColumns))
	assert.Equal(t, "AAPL", row[0])
	assert.Equal(t, false, row[3], `isdelisted "N" -> false`)
	assert.Equal(t, "SF1", row[7])
	assert.Equal(t, time.Date(1986, 1, 1, 0, 0, 0, 0, time.UTC), row[8])
	assert.Nil(t, row[9], "NULL lastpricedate = still active")
	assert.Nil(t, row[10])

	// Empty-string lastpricedate coerces to NULL (spec §2.5: reader must
	// accept both "" and NaT for active tickers).
	r = apiRow(tickersAPICols, []any{"SPY", "SPDR S&P 500", "NYSEARCA", "N", "ETF",
		nil, nil, "SFP", "1993-01-29", "", ""})
	row, _, err = convertTickerAPIRow(r)
	require.NoError(t, err)
	assert.Nil(t, row[9])
	assert.Nil(t, row[5], "null sector -> NULL")

	// Delisted stock keeps its delistdate when the API provides one
	// (locked decision 3: read 'delistdate', never the 'delistedate' typo).
	r = apiRow(tickersAPICols, []any{"DEAD", "Dead Co", "NYSE", "Y", "Domestic Common Stock",
		nil, nil, "SF1", "2010-01-04", "2024-03-15", "2024-03-15"})
	row, _, err = convertTickerAPIRow(r)
	require.NoError(t, err)
	assert.Equal(t, true, row[3])
	assert.Equal(t, time.Date(2024, 3, 15, 0, 0, 0, 0, time.UTC), row[10])

	// The dead 'delistedate' spelling is ignored even if present.
	cols := append(append([]string{}, tickersAPICols[:10]...), "delistedate")
	r = apiRow(cols, []any{"X", "X Co", "NYSE", "N", "Domestic Common Stock",
		nil, nil, "SF1", "2010-01-04", nil, "2024-03-15"})
	row, _, err = convertTickerAPIRow(r)
	require.NoError(t, err)
	assert.Nil(t, row[10], "delistedate (typo column) must not populate delist_date")
}

func TestConvertTickerAPIRowErrors(t *testing.T) {
	// Bad table.
	r := apiRow(tickersAPICols, []any{"X", nil, nil, "N", nil, nil, nil, "SF2", nil, nil, nil})
	_, _, err := convertTickerAPIRow(r)
	assert.ErrorIs(t, err, errSkipRow)

	// Bad isdelisted.
	r = apiRow(tickersAPICols, []any{"X", nil, nil, "maybe", nil, nil, nil, "SF1", nil, nil, nil})
	_, _, err = convertTickerAPIRow(r)
	assert.ErrorIs(t, err, errSkipRow)
}

func TestConvertSF1APIRow(t *testing.T) {
	cols := []string{"ticker", "dimension", "calendardate", "datekey", "reportperiod",
		"fiscalperiod", "lastupdated", "marketcap", "revenue"}
	r := apiRow(cols, []any{"AAPL", "MRT", "2024-03-31", "2024-05-03", "2024-03-30",
		"Q2", "2024-05-04", 3.4e12, nil})
	row, nulled, err := convertSF1APIRow(r)
	require.NoError(t, err)
	assert.Equal(t, 0, nulled)
	require.Len(t, row, len(sf1Columns))

	assert.Equal(t, "AAPL", row[0])
	assert.Equal(t, "MRT", row[1])
	assert.Equal(t, time.Date(2024, 5, 3, 0, 0, 0, 0, time.UTC), row[3], "datekey")
	assert.Equal(t, "Q2", row[5])

	// marketcap passes through as raw USD float (spec §2.3); absent metric
	// columns are NULL.
	mcIdx, revIdx := -1, -1
	for i, name := range sf1Columns {
		switch name {
		case "marketcap":
			mcIdx = i
		case "revenue":
			revIdx = i
		}
	}
	require.GreaterOrEqual(t, mcIdx, 0)
	assert.Equal(t, 3.4e12, row[mcIdx])
	assert.Nil(t, row[revIdx])

	// Invalid dimension rejected.
	r = apiRow(cols, []any{"AAPL", "XXX", nil, "2024-05-03", nil, nil, nil, nil, nil})
	_, _, err = convertSF1APIRow(r)
	assert.ErrorIs(t, err, errSkipRow)

	// Missing datekey rejected.
	r = apiRow(cols, []any{"AAPL", "MRT", nil, nil, nil, nil, nil, nil, nil})
	_, _, err = convertSF1APIRow(r)
	assert.ErrorIs(t, err, errSkipRow)
}

func TestConvertEventAPIRow(t *testing.T) {
	cols := []string{"ticker", "date", "eventcodes"}

	row, _, err := convertEventAPIRow(apiRow(cols, []any{"AAPL", "2024-05-02", "22|71"}))
	require.NoError(t, err)
	assert.Equal(t, []any{"AAPL", time.Date(2024, 5, 2, 0, 0, 0, 0, time.UTC), "22|71"}, row)

	// Numeric single-code cell round-trips as its integer string.
	row, _, err = convertEventAPIRow(apiRow(cols, []any{"AAPL", "2024-05-02", float64(13)}))
	require.NoError(t, err)
	assert.Equal(t, "13", row[2])

	_, _, err = convertEventAPIRow(apiRow(cols, []any{"AAPL", "2024-05-02", nil}))
	assert.ErrorIs(t, err, errSkipRow)
	_, _, err = convertEventAPIRow(apiRow(cols, []any{"AAPL", nil, "22"}))
	assert.ErrorIs(t, err, errSkipRow)
}
