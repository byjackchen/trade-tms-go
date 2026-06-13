package sharadar

// syncplan_test.go pins the pure planning layer to the Python reference
// oracles (test_writer_sep.py for _date_chunks, test_writer_tickers.py for
// the survivorship filter, catchup.py semantics for trading days).

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/byjackchen/trade-tms-go/internal/data/calendar"
)

func d(y int, m time.Month, day int) calendar.Date { return calendar.NewDate(y, m, day) }

func TestDateChunksFullYearQuarters(t *testing.T) {
	// Oracle test_writer_sep.py:64-77: a full year with months=3 yields
	// exactly the four calendar quarters.
	chunks, err := dateChunks(d(2023, time.January, 1), d(2023, time.December, 31), 3)
	require.NoError(t, err)
	assert.Equal(t, []dateRange{
		{Start: d(2023, time.January, 1), End: d(2023, time.March, 31)},
		{Start: d(2023, time.April, 1), End: d(2023, time.June, 30)},
		{Start: d(2023, time.July, 1), End: d(2023, time.September, 30)},
		{Start: d(2023, time.October, 1), End: d(2023, time.December, 31)},
	}, chunks)
}

func TestDateChunksMidQuarterStartAndClamp(t *testing.T) {
	// A start mid-quarter yields a partial first chunk ending at that
	// quarter's boundary; the last chunk clamps to end (spec §6.1).
	chunks, err := dateChunks(d(2023, time.February, 15), d(2023, time.May, 10), 3)
	require.NoError(t, err)
	assert.Equal(t, []dateRange{
		{Start: d(2023, time.February, 15), End: d(2023, time.March, 31)},
		{Start: d(2023, time.April, 1), End: d(2023, time.May, 10)},
	}, chunks)
}

func TestDateChunksCrossYear(t *testing.T) {
	chunks, err := dateChunks(d(2023, time.November, 15), d(2024, time.February, 10), 3)
	require.NoError(t, err)
	assert.Equal(t, []dateRange{
		{Start: d(2023, time.November, 15), End: d(2023, time.December, 31)},
		{Start: d(2024, time.January, 1), End: d(2024, time.February, 10)},
	}, chunks)
}

func TestDateChunksSingleDayAndEmpty(t *testing.T) {
	chunks, err := dateChunks(d(2024, time.June, 12), d(2024, time.June, 12), 3)
	require.NoError(t, err)
	assert.Equal(t, []dateRange{{Start: d(2024, time.June, 12), End: d(2024, time.June, 12)}}, chunks)

	chunks, err = dateChunks(d(2024, time.June, 13), d(2024, time.June, 12), 3)
	require.NoError(t, err)
	assert.Empty(t, chunks)
}

func TestDateChunksMonthsValidation(t *testing.T) {
	// MUST-MATCH error message shape (spec §6.1).
	for _, months := range []int{0, 5, 7, 13, -1} {
		_, err := dateChunks(d(2023, time.January, 1), d(2023, time.December, 31), months)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "months must divide 12 evenly; got")
	}
	for _, months := range []int{1, 2, 3, 4, 6, 12} {
		_, err := dateChunks(d(2023, time.January, 1), d(2023, time.December, 31), months)
		assert.NoError(t, err, "months=%d", months)
	}
}

func TestDateChunksTwelveMonths(t *testing.T) {
	chunks, err := dateChunks(d(2022, time.March, 5), d(2024, time.January, 15), 12)
	require.NoError(t, err)
	assert.Equal(t, []dateRange{
		{Start: d(2022, time.March, 5), End: d(2022, time.December, 31)},
		{Start: d(2023, time.January, 1), End: d(2023, time.December, 31)},
		{Start: d(2024, time.January, 1), End: d(2024, time.January, 15)},
	}, chunks)
}

func TestTradingDays(t *testing.T) {
	cal, err := calendar.NewNYSE()
	require.NoError(t, err)

	// 2026-06-12 (Fri) .. 2026-06-16 (Tue): weekend skipped.
	days := tradingDays(cal, d(2026, time.June, 12), d(2026, time.June, 16))
	assert.Equal(t, []calendar.Date{
		d(2026, time.June, 12), d(2026, time.June, 15), d(2026, time.June, 16),
	}, days)

	// NYSE holiday skipped ([IMPROVE] over bdate_range): 2026-01-01 is New
	// Year's Day (Thursday).
	days = tradingDays(cal, d(2025, time.December, 31), d(2026, time.January, 2))
	assert.Equal(t, []calendar.Date{
		d(2025, time.December, 31), d(2026, time.January, 2),
	}, days)

	// end < start -> empty (spec §8.2 step 4).
	assert.Empty(t, tradingDays(cal, d(2026, time.June, 12), d(2026, time.June, 11)))

	// Outside the calendar range: weekday fallback (bdate_range parity).
	// 2031-01-04 is a Saturday.
	days = tradingDays(cal, d(2031, time.January, 3), d(2031, time.January, 6))
	assert.Equal(t, []calendar.Date{
		d(2031, time.January, 3), d(2031, time.January, 6),
	}, days)
}

func TestCatchupWindow(t *testing.T) {
	cal, err := calendar.NewNYSE()
	require.NoError(t, err)
	ny := cal.Location()

	// A sync at 2026-06-10 01:30 UTC is 2026-06-09 21:30 in New York: the
	// watermark date is the NY date (locked decision 2), and target = T-1.
	last := time.Date(2026, time.June, 10, 1, 30, 0, 0, time.UTC)
	start, target := catchupWindow(last, d(2026, time.June, 12), ny)
	assert.Equal(t, d(2026, time.June, 9), start)
	assert.Equal(t, d(2026, time.June, 11), target)
}

func TestBatchTickers(t *testing.T) {
	var tickers []string
	for i := 0; i < 1050; i++ {
		tickers = append(tickers, "T")
	}
	batches := batchTickers(tickers, sf1TickerBatchSize)
	require.Len(t, batches, 3)
	assert.Len(t, batches[0], 500)
	assert.Len(t, batches[1], 500)
	assert.Len(t, batches[2], 50)

	assert.Nil(t, batchTickers(nil, 500))
	assert.Nil(t, batchTickers([]string{"A"}, 0))

	exact := batchTickers([]string{"A", "B"}, 2)
	require.Len(t, exact, 1)
	assert.Equal(t, []string{"A", "B"}, exact[0])
}

func TestKeepTickerRowOracle(t *testing.T) {
	// Mirrors the 7-row fixture of test_writer_tickers.py:20-71 — exactly
	// AAPL, ACWX, DEAD, MSFT, SPY survive.
	cases := []struct {
		ticker, table, category, isdelisted string
		keep                                bool
	}{
		{"AAPL", "SF1", "Domestic Common Stock", "N", true},
		{"MSFT", "SF1", "Domestic Common Stock Primary Class", "N", true},
		{"DEAD", "SF1", "Domestic Common Stock", "Y", true},     // delisted stocks kept
		{"PREF", "SF1", "Domestic Preferred Stock", "N", false}, // non-common dropped
		{"SPY", "SFP", "ETF", "N", true},
		{"ACWX", "SFP", "ETF", "N", true},
		{"DEADETF", "SFP", "ETF", "Y", false}, // delisted funds dropped
		{"NOCAT", "SF1", "", "N", false},      // NaN category -> "" -> dropped
		{"OTHER", "SF3", "Domestic Common Stock", "N", false},
	}
	var kept []string
	for _, tc := range cases {
		got := keepTickerRow(tc.table, tc.category, tc.isdelisted)
		assert.Equal(t, tc.keep, got, "%s", tc.ticker)
		if got {
			kept = append(kept, tc.ticker)
		}
	}
	assert.Equal(t, []string{"AAPL", "MSFT", "DEAD", "SPY", "ACWX"}, kept)
}
